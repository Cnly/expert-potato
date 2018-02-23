package core

import (
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"sync"
)

// EConn: End Connections
// ECM: EConnManager

// TODO: Handle timeout
// TODO: Use channel for EConn finalization?

const (
	closeTypeRemote = "remote"
	closeTypeLocal  = "local"
)

// No 0 because 0 implies the first packet
func increaseSeq(u *uint32) {
	if *u == math.MaxUint32 {
		*u = 1
	} else {
		*u++
	}
}

// Only create new EConns from ECM
type eConn struct {
	eConnManager        *eConnManager
	conn                net.Conn
	writeBuffer         map[uint32][]byte
	writeBufMutex       sync.Mutex
	writeBufCond        sync.Cond
	readBuffer          [][]byte
	readBufMutex        sync.Mutex
	readBufCond         sync.Cond
	readQueueLength     int
	maxPacketBodyLength int
	id                  uint16
	nextLocalSeq        uint32 // The sequence value used when writing to PCM
	nextRemoteSeq       uint32 // Used to keep track of packets received from PCM
	dieCtl              dieCtl
	finalSeq            uint32 // The sequence value that indicates end of stream
}

// This function should only be invoked by ECM.
// Create new EConns from ECM.
func newEConn(conn net.Conn, id uint16, readQueueLength int, maxPacketBodyLength int) *eConn {
	ec := eConn{
		conn:                conn,
		writeBuffer:         make(map[uint32][]byte),
		readBuffer:          make([][]byte, 0),
		readQueueLength:     readQueueLength,
		maxPacketBodyLength: maxPacketBodyLength,
		id:                  id,
	}
	ec.readBufCond = sync.Cond{L: &ec.readBufMutex}
	ec.writeBufCond = sync.Cond{L: &ec.writeBufMutex}
	return &ec
}

func (ec *eConn) write(seq uint32, b []byte) {
	if ec.dieCtl.isClosedWithType(closeTypeLocal) {
		return
	}
	ec.writeBufMutex.Lock()
	ec.writeBuffer[seq] = b
	ec.writeBufCond.Signal()
	ec.writeBufMutex.Unlock()
}

func (ec *eConn) appendToReadBuffer(b []byte) {
	if ec.dieCtl.isClosedWithType(closeTypeRemote) {
		return
	}

	ec.readBufMutex.Lock()
	defer ec.readBufMutex.Unlock()
	for len(ec.readBuffer) >= ec.readQueueLength {
		if ec.dieCtl.isClosedWithType(closeTypeRemote) {
			return
		}
		ec.readBufCond.Wait()
	}
	ec.readBuffer = append(ec.readBuffer, b)
	ec.readBufCond.Signal()
}

func (ec *eConn) start() {
	go func() {
		maxPacketBodyLength := ec.maxPacketBodyLength
		for {
			if ec.dieCtl.isClosed() { // Handles finalization from remote and local
				return
			}

			buf := make([]byte, maxPacketBodyLength)
			n, err := ec.conn.Read(buf)
			if err != nil {
				if ec.dieCtl.isClosed() { // Handles finalization from local
					return
				} else if err == io.EOF {
					log.Printf("EConn disconnected by peer %s (EConn id: %d)", ec.conn.RemoteAddr(), ec.id)
					ec.initiateFinalization()
					return
				} else {
					log.Printf("error reading from EConn (id: %d); addr: %s, err: %v", ec.id, ec.conn.RemoteAddr(), err)
					ec.initiateFinalization()
					return
				}
			}
			buf = buf[:n]
			ec.appendToReadBuffer(buf)
		}
	}()

	go func() {
		writeToPCM := ec.eConnManager.core.pConnManager.write
		for {
			if ec.dieCtl.isClosedWithType(closeTypeRemote) { // Handles finalization from remote
				return
			}

			ec.readBufMutex.Lock()
			for len(ec.readBuffer) == 0 {
				if ec.dieCtl.isClosed() {
					if ec.dieCtl.isClosedWithType(closeTypeLocal) { // Handles finalization from local
						ec.eConnManager.core.pConnManager.sendFinalizeEConn(ec.id, ec.nextLocalSeq, false)
					}
					ec.readBufMutex.Unlock()
					return
				}

				ec.readBufCond.Wait()
			}
			b := ec.readBuffer[0]
			ec.readBuffer[0] = nil
			ec.readBuffer = ec.readBuffer[1:]
			ec.readBufCond.Signal()
			ec.readBufMutex.Unlock()

			writeToPCM(ec.id, ec.nextLocalSeq, b)
			increaseSeq(&ec.nextLocalSeq)
		}
	}()

	go func() {
		firstLoop := true
		ec.writeBufMutex.Lock()
		defer ec.writeBufMutex.Unlock()
		for {
			if firstLoop {
				// We don't wait in the first loop - there may be data in buffer before this goroutine started
				firstLoop = false
			} else {
				ec.writeBufCond.Wait()
			}
			for seq := ec.nextRemoteSeq; ; increaseSeq(&seq) {
				if ec.dieCtl.isClosedWithType(closeTypeRemote) && seq == ec.finalSeq { // Handles finalization from remote
					ec.conn.Close()
					ec.eConnManager.removeEConn(ec)
					return
				}

				b, ok := ec.writeBuffer[seq]
				if !ok {
					break
				}
				//t := time.Now()
				//ec.conn.SetWriteDeadline(t.Add(1 * time.Second))
				_, err := ec.conn.Write(b)
				if err != nil {
					if ec.dieCtl.isClosed() { // Handles finalization from local
						return
					}
					log.Printf("error writing to EConn (id: %d) (%v)", ec.id, err)
					ec.initiateFinalization()
					return
				}
				increaseSeq(&ec.nextRemoteSeq)
				delete(ec.writeBuffer, seq)
			}
		}
	}()
}

// Invoked when the local peer of EConn disconnects.
func (ec *eConn) initiateFinalization() {
	withLock(&ec.dieCtl.Mutex, func() {
		ec.conn.Close()
		ec.eConnManager.removeEConn(ec)

		if ec.dieCtl.closed {
			return
		}

		ec.dieCtl.closed = true
		ec.dieCtl.closeType = closeTypeLocal

		go func() { // Execute these in a goroutine to release the lock on dieCtl
			ec.writeBufMutex.Lock() // Ensure the following signal is received
			ec.writeBufCond.Signal()
			ec.writeBufMutex.Unlock()

			ec.readBufMutex.Lock()
			ec.readBufCond.Signal()
			ec.readBufMutex.Unlock()
		}()
	})

}

func (ec *eConn) onReceiveRemoteFinalization(finalSeq uint32, immediate bool) {
	withLock(&ec.dieCtl.Mutex, func() {
		if ec.dieCtl.closed && !immediate {
			return
		}
		ec.dieCtl.closed = true
		ec.dieCtl.closeType = closeTypeRemote
		if !immediate {
			ec.finalSeq = finalSeq
		}

		go func() { // Execute these in a goroutine to release the lock on dieCtl
			ec.writeBufMutex.Lock() // Ensure the following signal is received
			if immediate {
				ec.finalSeq = ec.nextRemoteSeq
			}
			ec.writeBufCond.Signal()
			ec.writeBufMutex.Unlock()

			ec.readBufMutex.Lock()
			ec.readBufCond.Signal()
			ec.readBufMutex.Unlock()
		}()
	})
}

type eConnManager struct {
	core             *Core
	listener         net.Listener
	eConnIdMap       map[*eConn]uint16
	idEConnMap       map[uint16]*eConn
	eConnIdMapsMutex sync.Mutex
	closed           bool
	connChannel      chan net.Conn
	dieChannel       chan bool
	idCtl            struct {
		sync.Mutex
		nextId uint16
	}
}

func newEConnManager(core *Core) *eConnManager {
	ecm := eConnManager{
		core:        core,
		eConnIdMap:  make(map[*eConn]uint16),
		idEConnMap:  make(map[uint16]*eConn),
		connChannel: make(chan net.Conn),
		dieChannel:  make(chan bool),
	}
	nextId := uint16(rand.Uint32() >> 16)
	if nextId == 0 {
		nextId = 1
	}
	ecm.idCtl.nextId = nextId
	return &ecm
}

func (ecm *eConnManager) write(id uint16, seq uint32, b []byte) {
	if id == 0 {
		log.Printf("caught attempt to write to ECM with id 0 (illegal); rejecting")
		return
	}

	ecm.eConnIdMapsMutex.Lock()
	ec, ok := ecm.idEConnMap[id]
	ecm.eConnIdMapsMutex.Unlock()
	if !ok {
		switch ecm.core.position {
		case CLIENT:
			log.Printf("PCM attempted to write to a non-existing EConn (id: %d, seq: %d); rejecting", id, seq)
			ecm.core.pConnManager.sendFinalizeEConn(id, 0, true)
			return
		case SERVER:
			if seq != 0 {
				// Not a new connection - we're receiving data for a finalized and removed EConn
				log.Printf("PCM attempted to write to an unknown or finalized EConn (id: %d, seq: %d); rejecting", id, seq)
				ecm.core.pConnManager.sendFinalizeEConn(id, 0, true)
				return
			}

			conn, err := net.Dial("tcp", ecm.core.config.DestAddr)
			if err != nil {
				log.Printf("error establishing connection to destination (%v)", err)
				ecm.core.pConnManager.sendFinalizeEConn(id, 0, true)
				return
			}
			ec = ecm.createEConn(conn, id)
			ec.start()
			log.Printf("created EConn to destination; id: %d", id)
		}

	}
	ec.write(seq, b)
}

func (ecm *eConnManager) getNewId() uint16 {
	idCtl := &ecm.idCtl
	idCtl.Lock()
	defer idCtl.Unlock()
	id := idCtl.nextId
	if id == math.MaxUint16 {
		idCtl.nextId = 1
	} else {
		idCtl.nextId++
	}
	return id
}

// id == 0 stands for new id (auto generated)
func (ecm *eConnManager) createEConn(conn net.Conn, id uint16) *eConn {
	if id == 0 {
		id = ecm.getNewId()
	}

	ec := newEConn(conn, id, ecm.core.config.EConnReadQueueLength, int(ecm.core.config.MaxPacketBodyLength))
	ec.eConnManager = ecm

	ecm.eConnIdMapsMutex.Lock()
	ecm.eConnIdMap[ec] = id
	ecm.idEConnMap[id] = ec
	ecm.eConnIdMapsMutex.Unlock()

	return ec
}

func (ecm *eConnManager) onReceiveRemoteFinalization(id uint16, finalSeq uint32, immediate bool) {
	if id == 0 {
		log.Printf("caught attempt from PCM to finalize EConn with id 0 (illegal); rejecting")
		return
	}

	ecm.eConnIdMapsMutex.Lock()
	ec, ok := ecm.idEConnMap[id]
	ecm.eConnIdMapsMutex.Unlock()
	if !ok {
		log.Printf("PCM is trying to finalize non-existing EConn (id: %d); ignoring", id)
		return
	}

	log.Printf("finalizing EConn (id: %d) at remote's request (immediate: %t)", id, immediate)
	ec.onReceiveRemoteFinalization(finalSeq, immediate)
}

// Removes an EConn from ECM.
func (ecm *eConnManager) removeEConn(ec *eConn) {
	id := ec.id
	ecm.eConnIdMapsMutex.Lock()
	delete(ecm.eConnIdMap, ec)
	delete(ecm.idEConnMap, id)
	ecm.eConnIdMapsMutex.Unlock()
}

func (ecm *eConnManager) start(position Position) {
	config := ecm.core.config

	if position == CLIENT {
		ln, err := net.Listen("tcp", config.EConnBindingAddr)
		if err != nil {
			log.Fatalf("error binding EConn listenting address (%v)", err)
		}
		ecm.listener = ln

		go func() {
			log.Println("ECM listening")
			for {
				conn, err := ln.Accept()
				if err != nil {
					if ecm.closed {
						return
					}
					log.Printf("error accepting EConn (%v)", err)
				}
				ecm.connChannel <- conn
			}
		}()

		go func() {
			for {
				select {
				case conn := <-ecm.connChannel:
					go func() {
						eConn := ecm.createEConn(conn, 0)
						eConn.start()
						log.Printf("accepted EConn from %s (id: %d)", conn.RemoteAddr(), eConn.id)
					}()
				case <-ecm.dieChannel:
					ecm.closed = true
					ecm.listener.Close()
					// TODO
				}
			}
		}()
	}
}

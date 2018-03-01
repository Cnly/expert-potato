package core

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// PConn: Parallel Connections, which are the connections shared between client and server
// PCM: PConnManager

// TODO: Handle timeout

type pConn struct {
	pConnManager *pConnManager
	conn         net.Conn
	dieCtl       dieCtl
}

func (pc *pConn) start(position Position) {
	tokenBytes := []byte(pc.pConnManager.core.config.PConnAuthToken)

	switch position {
	case CLIENT:
		challengeBuf := make([]byte, sha512.Size256)
		_, err := io.ReadFull(pc.conn, challengeBuf)
		if err != nil {
			log.Printf("error receiving challenge message from PConn server (%v); closing PConn", err)
			pc.close()
			return
		}

		responseByteArray := sha256.Sum256(concatByteSlices(challengeBuf, tokenBytes))
		response := responseByteArray[:]
		_, err = pc.conn.Write(response)
		if err != nil {
			log.Printf("error sending challenge response to PConn server (%v); closing PConn", err)
			pc.close()
			return
		}

		resultBuf := make([]byte, 1)
		_, err = io.ReadFull(pc.conn, resultBuf)
		if err != nil {
			log.Printf("error receiving challenge result from PConn server (%v); closing PConn", err)
			pc.close()
			return
		}
		if resultBuf[0] == 1 {
			pc.pConnManager.markAuthenticated(pc)
		} else {
			log.Fatalf("failed authenticating PConn to server; make sure server and client share the same token")
		}
	case SERVER:
		challengeByteSlices := concatByteSlices(uint64ByteArray(uint64(time.Now().UnixNano())), uint64ByteArray(rand.Uint64()))
		challengeByteArray := sha256.Sum256(challengeByteSlices)
		challengeBytes := challengeByteArray[:]
		_, err := pc.conn.Write(challengeBytes)
		if err != nil {
			log.Printf("error sending challenge message to PConn client (%v); closing PConn", err)
			pc.close()
			return
		}

		responseBuf := make([]byte, sha512.Size256)
		_, err = io.ReadFull(pc.conn, responseBuf)
		if err != nil {
			log.Printf("error receiving challenge response from PConn client (%v); closing PConn", err)
			pc.close()
			return
		}

		sendResult := func(success bool) {
			successByteSlice := []byte{0}
			if success {
				successByteSlice[0] = 1
			}
			_, err := pc.conn.Write(successByteSlice)
			if err != nil {
				log.Printf("error sending challenge result to PConn client (%v); closing PConn", err)
				pc.close()
				return
			}
		}
		expectedResponseByteArray := sha256.Sum256(concatByteSlices(challengeBytes, tokenBytes))
		expectedResponse := expectedResponseByteArray[:]
		if bytes.Equal(responseBuf, expectedResponse) {
			log.Printf("authenticated PConn from %s", pc.conn.RemoteAddr())
			sendResult(true)
			pc.pConnManager.markAuthenticated(pc)
		} else {
			log.Printf("failed authenticating PConn from %s; closing PConn", pc.conn.RemoteAddr())
			sendResult(false)
			pc.close()
			return
		}
	}

	go func() {
		writeToECM := pc.pConnManager.core.eConnManager.write
		defer pc.conn.Close()
		for {
			headerBuf := make([]byte, 8)
			_, err := io.ReadFull(pc.conn, headerBuf)
			if err != nil {
				log.Printf("error receiving header from PConn (%v); closing PConn", err)
				pc.close()
				return
			}
			id, seq, bodyLen := parseHeader(headerBuf)
			if bodyLen >= 65501 {
				switch bodyLen {
				case lenValueCloseEConn:
					go pc.pConnManager.core.eConnManager.onReceiveRemoteFinalization(id, seq, false)
				case lenValueCloseEConnImmediate:
					go pc.pConnManager.core.eConnManager.onReceiveRemoteFinalization(id, seq, true)
				case lenValueEConnStopRemote:
					fallthrough
				case lenValueEConnStartRemote:
					go pc.pConnManager.core.eConnManager.writeCtlPacket(id, seq, bodyLen, nil)
				default:
					log.Printf("received unknown special body length value for EConn (id: %d, seq: %d, bodyLen: %d); closing PConn", id, seq, bodyLen)
					// TODO: Handle unknown values?
					pc.close()
					return
				}
				continue
			}
			b := make([]byte, bodyLen)
			_, err = io.ReadFull(pc.conn, b)
			if err != nil {
				log.Printf("error receiving data body from PConn (%v); closing PConn", err)
				pc.close()
				return
			}
			go writeToECM(id, seq, b)
		}
	}()
}

func (pc *pConn) write(b []byte) (n int, err error) {
	n, err = pc.conn.Write(b)
	return n, err
}

func (pc *pConn) close() {
	if pc.dieCtl.isClosed() {
		return
	}
	pc.dieCtl.close()
	pc.conn.Close()
	if pc.pConnManager.core.position == CLIENT {
		localIPString := interface{}(pc.conn.LocalAddr()).(*net.TCPAddr).IP.String()
		go pc.pConnManager.dial(localIPString)
	}
}

type pConnManager struct {
	core            *Core
	idlePConns      []*pConn
	idlePConnsMutex sync.Mutex
	idlePConnsCond  sync.Cond
}

func newPConnManager(core *Core) *pConnManager {
	pcm := pConnManager{
		core: core,
	}
	pcm.idlePConnsCond = *sync.NewCond(&pcm.idlePConnsMutex)

	return &pcm
}

func (pcm *pConnManager) getIdlePConn() *pConn {
	pcm.idlePConnsMutex.Lock()
	defer pcm.idlePConnsMutex.Unlock()
	for len(pcm.idlePConns) == 0 {
		pcm.idlePConnsCond.Wait()
	}
	var pConn *pConn
	pConn, pcm.idlePConns = pcm.idlePConns[0], pcm.idlePConns[1:]
	return pConn
}

func (pcm *pConnManager) putIdlePConn(pc *pConn) {
	pcm.idlePConnsMutex.Lock()
	defer pcm.idlePConnsMutex.Unlock()
	pcm.idlePConns = append(pcm.idlePConns, pc)
	pcm.idlePConnsCond.Signal()
}

func (pcm *pConnManager) writeRaw0(b []byte) {
	for {
		pc := pcm.getIdlePConn()
		if pc.dieCtl.isClosed() {
			continue
		}
		_, err := pc.write(b)
		if err != nil {
			// Do not put PConn back if there is error
			log.Printf("error writing to PConn (%v); closing PConn", err)
			pc.close()
			continue
		}
		pcm.putIdlePConn(pc)
		return
	}
}

func (pcm *pConnManager) writeRaw(id uint16, seq uint32, lenValue uint16, body []byte) {
	idBytes := uint16ByteArray(id)
	seqBytes := uint32ByteArray(seq)
	lenBytes := uint16ByteArray(lenValue)
	if body == nil {
		pcm.writeRaw0(concatByteSlices(idBytes, seqBytes, lenBytes))
	} else {
		pcm.writeRaw0(concatByteSlices(idBytes, seqBytes, lenBytes, body))
	}
}

func (pcm *pConnManager) write(id uint16, seq uint32, b []byte) {
	pcm.writeRaw(id, seq, uint16(len(b)), b)
}

func (pcm *pConnManager) markAuthenticated(pc *pConn) {
	pcm.putIdlePConn(pc)
}

func (pcm *pConnManager) createPConn(conn net.Conn) *pConn {
	pConn := pConn{
		pConnManager: pcm,
		conn:         conn,
	}
	return &pConn
}

func (pcm *pConnManager) dial(localIPString string) {
	if strings.ContainsRune(localIPString, ':') {
		// IPv6
		localIPString = "[" + localIPString + "]"
	}
	localAddr := mustResolveTCPAddr(localIPString + ":0")

	dialer := net.Dialer{
		LocalAddr: localAddr,
		KeepAlive: pcm.core.config.PConnKeepAlive,
	}

	var interval time.Duration
	for interval = 0; ; interval++ {
		time.Sleep(interval * time.Second)
		log.Printf("initiating PConn to server from local %s", localAddr)
		conn, err := dialer.Dial("tcp", pcm.core.config.ServerAddr)
		if err != nil {
			log.Printf("error establishing PConn with server; local: %s, error: %v", localAddr, err)
			continue
		}
		pc := pcm.createPConn(conn)
		pc.start(CLIENT)
		log.Printf("PConn established from local addr %s", conn.LocalAddr())
		return
	}
}

// Notify the remote to finalize an EConn
func (pcm *pConnManager) sendFinalizeEConn(id uint16, seq uint32, immediate bool) {
	var lenValue uint16
	if immediate {
		lenValue = lenValueCloseEConnImmediate
	} else {
		lenValue = lenValueCloseEConn
	}
	pcm.writeRaw(id, seq, lenValue, nil)
}

func (pcm *pConnManager) start(position Position) {
	config := pcm.core.config
	switch position {
	case CLIENT:
		for _, laddrString := range config.PConnBindingAddresses {
			go func(laddrString string) {
				go pcm.dial(laddrString)
			}(laddrString)
		}
	case SERVER:
		ln, err := net.ListenTCP("tcp", mustResolveTCPAddr(config.ServerAddr))
		if err != nil {
			log.Fatalf("error binding server address (%v)", err)
		}
		go func() {
			log.Println("PCM listening")
			for {
				conn, err := ln.AcceptTCP()
				if err != nil {
					log.Printf("error handling incoming PConn (%v)", err)
				} else {
					go func(conn net.Conn) {
						pc := pcm.createPConn(conn)
						pc.start(SERVER)
					}(conn)
					log.Printf("accepted incoming PConn from %s", conn.RemoteAddr())
				}
			}
		}()
	}
}

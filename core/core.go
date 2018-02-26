package core

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"math"
	"net"
	"sync"
	"time"
)

type Position int

const (
	CLIENT Position = iota
	SERVER
)

const (
	lenValueCloseEConn uint16 = math.MaxUint16 - iota
	lenValueCloseEConnImmediate
	lenValueEConnStopRemote
	lenValueEConnStartRemote
)

var bin binary.ByteOrder = binary.BigEndian

func uint16ByteArray(u uint16) []byte {
	b := make([]byte, 2)
	bin.PutUint16(b, u)
	return b
}

func uint32ByteArray(u uint32) []byte {
	b := make([]byte, 4)
	bin.PutUint32(b, u)
	return b
}

func uint64ByteArray(u uint64) []byte {
	b := make([]byte, 8)
	bin.PutUint64(b, u)
	return b
}

// Does not work for PConnBindingAddresses
func mustResolveTCPAddr(addrString string) *net.TCPAddr {
	addr, err := net.ResolveTCPAddr("tcp", addrString)
	if err != nil {
		log.Fatalf("error processing TCP address: %s (%v)", addrString, err)
	}
	return addr
}

func concatByteSlices(slices ...[]byte) []byte {
	b := make([]byte, 0, 8)
	for _, slice := range slices {
		b = append(b, slice...)
	}
	return b
}

func parseHeader(b []byte) (id uint16, seq uint32, len uint16) {
	return bin.Uint16(b[:2]), bin.Uint32(b[2:6]), bin.Uint16(b[6:8])
}

func withLock(m *sync.Mutex, f func()) {
	m.Lock()
	defer m.Unlock()
	f()
}

type dieCtl struct {
	closed    bool
	closeType string
	sync.Mutex
}

func (dc *dieCtl) isClosed() bool {
	dc.Lock()
	defer dc.Unlock()
	return dc.closed
}

func (dc *dieCtl) isClosedWithType(t string) bool {
	dc.Lock()
	defer dc.Unlock()
	if dc.closed && dc.closeType == t {
		return true
	}
	return false
}

func (dc *dieCtl) getCloseType() string {
	dc.Lock()
	defer dc.Unlock()
	return dc.closeType
}

func (dc *dieCtl) close() {
	dc.Lock()
	defer dc.Unlock()
	dc.closed = true
}

func (dc *dieCtl) closeWithType(t string) {
	dc.Lock()
	defer dc.Unlock()
	dc.closed = true
	dc.closeType = t
}

type Config struct {
	MaxPacketBodyLength int
	EConnWriteConfig    struct {
		WriteBufMaxLen               int
		WriteBufStopRemoteThreshold  int
		WriteBufStartRemoteThreshold int
		OptimizationMode             bool
	}
	PConnAuthToken        string
	PConnBindingAddresses []string
	PConnKeepAlive        time.Duration
	EConnBindingAddr      string
	ServerAddr            string
	DestAddr              string
}

func NewConfigFromFile(filename string) (*Config, error) {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	config := Config{}
	err = json.Unmarshal(b, &config)
	if err != nil {
		return nil, err
	}
	errorOccurred := false
	if config.MaxPacketBodyLength == 0 || config.MaxPacketBodyLength >= 65501 {
		log.Println("MaxPacketBodyLength can only be between 0 and 65501 (exclusive)")
		errorOccurred = true
	}
	ecwc := config.EConnWriteConfig
	if !(0 <= ecwc.WriteBufStartRemoteThreshold) {
		log.Println("EConnWriteConfig.WriteBufStartRemoteThreshold cannot be negative")
		errorOccurred = true
	}
	if !(ecwc.WriteBufStartRemoteThreshold <= ecwc.WriteBufStopRemoteThreshold) {
		log.Println("EConnWriteConfig.WriteBufStopRemoteThreshold cannot be less than EConnWriteConfig.WriteBufStartRemoteThreshold")
		errorOccurred = true
	}
	if !(ecwc.WriteBufStopRemoteThreshold <= ecwc.WriteBufMaxLen) {
		log.Println("EConnWriteConfig.WriteBufMaxLen cannot be less than EConnWriteConfig.WriteBufStopRemoteThreshold")
		errorOccurred = true
	}
	if ecwc.WriteBufMaxLen == 0 {
		log.Println("EConnWriteConfig.WriteBufMaxLen cannot be 0")
		errorOccurred = true
	}
	if config.PConnAuthToken == "" {
		log.Println("PConnAuthToken cannot be empty; set it to a random value")
		errorOccurred = true
	}
	if len(config.PConnBindingAddresses) == 0 {
		log.Println("len(PConnBindingAddresses) == 0; check config file")
		errorOccurred = true
	}

	if errorOccurred {
		return nil, errors.New("one or more config value(s) is(are) illegal; see above")
	} else {
		return &config, nil
	}
}

type Core struct {
	position         Position
	swapECMPositions bool
	config           *Config
	pConnManager     *pConnManager
	eConnManager     *eConnManager
}

func NewCore(position Position, swapECMPositions bool, config *Config) *Core {
	core := Core{
		config:           config,
		position:         position,
		swapECMPositions: swapECMPositions,
	}
	core.pConnManager = newPConnManager(&core)
	core.eConnManager = newEConnManager(&core)

	return &core
}

func (core *Core) Start() {
	core.pConnManager.start(core.position)
	ecmPosition := core.position
	if core.swapECMPositions {
		ecmPosition = map[Position]Position{CLIENT: SERVER, SERVER: CLIENT}[ecmPosition]
	}
	core.eConnManager.start(ecmPosition)
}

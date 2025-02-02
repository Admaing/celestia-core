package load

import (
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/p2p/conn"
	"github.com/tendermint/tendermint/pkg/trace"
	protomem "github.com/tendermint/tendermint/proto/tendermint/mempool"
)

const (
	FirstChannel   = byte(0xee)
	SecondChannel  = byte(0x02)
	ThirdChannel   = byte(0x03)
	FourthChannel  = byte(0x04)
	FifthChannel   = byte(0x05)
	SixthChannel   = byte(0x06)
	SeventhChannel = byte(0x07)
	EighthChannel  = byte(0x08)
	NinthChannel   = byte(0x09)
	TenthChannel   = byte(0x10)
)

var priorities = make(map[byte]int)

func init() {
	for _, ch := range DefaultTestChannels {
		priorities[ch.ID] = ch.Priority
	}
}

var DefaultTestChannels = []*p2p.ChannelDescriptor{
	{
		ID:                  FirstChannel,
		Priority:            1,
		SendQueueCapacity:   1000,
		RecvBufferCapacity:  100000,
		RecvMessageCapacity: 20000000000,
		MessageType:         &protomem.TestTx{},
	},
	//{
	//	ID:                  SecondChannel,
	//	Priority:            3,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  1000,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  ThirdChannel,
	//	Priority:            5,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  FourthChannel,
	//	Priority:            7,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  FifthChannel,
	//	Priority:            9,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  SixthChannel,
	//	Priority:            11,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  SeventhChannel,
	//	Priority:            13,
	//	SendQueueCapacity:   100,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  EighthChannel,
	//	Priority:            15,
	//	SendQueueCapacity:   100,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 200000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  NinthChannel,
	//	Priority:            13,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
	//{
	//	ID:                  TenthChannel,
	//	Priority:            15,
	//	SendQueueCapacity:   1,
	//	RecvBufferCapacity:  100,
	//	RecvMessageCapacity: 2000000,
	//	MessageType:         &protomem.TestTx{},
	//},
}

var defaultMsgSizes = []int{
	300,
	1000,
	1000,
	100,
	1000,
	1000,
	100,
	100000,
	300,
	1000,
}

// MockReactor represents a mock implementation of the Reactor interface.
type MockReactor struct {
	p2p.BaseReactor
	channels []*conn.ChannelDescriptor

	mtx      sync.Mutex
	peers    map[p2p.ID]p2p.Peer
	received atomic.Int64
	metrics  metrics
	size     atomic.Int64

	tracer trace.Tracer
}

type metrics struct {
	mtx                     sync.Mutex
	startDownloadTime       map[string]time.Time
	cumulativeReceivedBytes map[string]int
	downloadSpeed           map[string]float64
	startUploadTime         map[string]time.Time
	cumulativeUploadBytes   map[string]int
	uploadSpeed             map[string]float64
}

// NewMockReactor creates a new mock reactor.
func NewMockReactor(channels []*conn.ChannelDescriptor, msgSize int) *MockReactor {
	s := atomic.Int64{}
	s.Store(int64(msgSize))
	mr := &MockReactor{
		channels: channels,
		peers:    make(map[p2p.ID]p2p.Peer),
		metrics: metrics{
			startDownloadTime:       map[string]time.Time{},
			downloadSpeed:           map[string]float64{},
			cumulativeReceivedBytes: map[string]int{},
			startUploadTime:         map[string]time.Time{},
			cumulativeUploadBytes:   map[string]int{},
			uploadSpeed:             map[string]float64{},
		},
		size: s,
	}
	mr.BaseReactor = *p2p.NewBaseReactor("MockReactor", mr)
	return mr
}

func (mr *MockReactor) SetTracer(tracer trace.Tracer) {
	mr.tracer = tracer
}

func (mr *MockReactor) IncreaseSize(s int64) {
	mr.size.Store(s)
}

// GetChannels implements Reactor.
func (mr *MockReactor) GetChannels() []*conn.ChannelDescriptor {
	return mr.channels
}

// InitPeer implements Reactor.
func (mr *MockReactor) InitPeer(peer p2p.Peer) p2p.Peer {
	// Initialize any data structures related to the peer here.
	// This is a mock implementation, so we'll keep it simple.
	return peer
}

// AddPeer implements Reactor.
func (mr *MockReactor) AddPeer(peer p2p.Peer) {
	mr.mtx.Lock()
	defer mr.mtx.Unlock()
	mr.peers[peer.ID()] = peer
	mr.FloodChannel(peer.ID(), 10*60*time.Minute, FirstChannel)
}

// RemovePeer implements Reactor.
func (mr *MockReactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	// Handle the removal of a peer.
	// In this mock implementation, we'll simply log the event.
	mr.Logger.Info("MockReactor removed a peer", "peer", peer.ID(), "reason", reason)
}

const mebibyte = 1_048_576

func (mr *MockReactor) PrintSpeeds() {
	mr.metrics.mtx.Lock()
	for _, peer := range mr.peers {
		cumul := mr.metrics.cumulativeReceivedBytes[string(peer.ID())]
		speed := mr.metrics.downloadSpeed[string(peer.ID())]
		fmt.Printf("%s: %d bytes received in speed %.2f mib/s\n", peer.ID(), cumul, speed/mebibyte)
	}
	total := float64(0)
	for _, speed := range mr.metrics.downloadSpeed {
		total += speed
	}
	fmt.Printf("total bandwidth download speed reached %.2f mib/s\n\n", total/mebibyte)

	for _, peer := range mr.peers {
		cumul := mr.metrics.cumulativeUploadBytes[string(peer.ID())]
		speed := mr.metrics.uploadSpeed[string(peer.ID())]
		fmt.Printf("%s: %d bytes sent in speed %.2f mib/s\n", peer.ID(), cumul, speed/mebibyte)
	}
	totalUpload := float64(0)
	for _, speed := range mr.metrics.uploadSpeed {
		totalUpload += speed
	}
	fmt.Printf("total bandwidth upload speed reached %.2f mib/s\n", totalUpload/mebibyte)
	fmt.Println("----------------------------------")
	fmt.Println()
	mr.metrics.mtx.Unlock()
}

// Receive implements Reactor.
func (mr *MockReactor) Receive(chID byte, peer p2p.Peer, msgBytes []byte) {
	msg := &protomem.Message{}
	err := proto.Unmarshal(msgBytes, msg)
	if err != nil {
		mr.Logger.Error("failure to unmarshal")
		// panic(err)
	}
	uw, err := msg.Unwrap()
	if err != nil {
		mr.Logger.Error("failure to unwrap")
		// panic(err)
	}
	mr.ReceiveEnvelope(p2p.Envelope{
		ChannelID: chID,
		Src:       peer,
		Message:   uw,
	})
}

type Payload struct {
	Time time.Time `json:"time"`
	Data string    `json:"data"`
}

// ReceiveEnvelope implements Reactor.
// It processes one of three messages: Txs, SeenTx, WantTx.
func (mr *MockReactor) ReceiveEnvelope(e p2p.Envelope) {
	switch msg := e.Message.(type) {
	case *protomem.TestTx:
		mr.metrics.mtx.Lock()
		if _, ok := mr.metrics.startDownloadTime[string(e.Src.ID())]; !ok {
			mr.metrics.startDownloadTime[string(e.Src.ID())] = time.Now()
		}
		mr.metrics.cumulativeReceivedBytes[string(e.Src.ID())] += len(msg.Tx)
		mr.metrics.downloadSpeed[string(e.Src.ID())] = float64(mr.metrics.cumulativeReceivedBytes[string(e.Src.ID())]) / time.Now().Sub(mr.metrics.startDownloadTime[string(e.Src.ID())]).Seconds()
		mr.metrics.mtx.Unlock()
	default:
		fmt.Printf("Unexpected message type %T\n", e.Message)
		return
	}
}

func (mr *MockReactor) SendBytes(id p2p.ID, chID byte, size int64) bool {
	mr.mtx.Lock()
	peer, has := mr.peers[id]
	mr.mtx.Unlock()
	if !has {
		mr.Logger.Error("Peer not found")
		return false
	}

	b := make([]byte, size)
	_, err := rand.Read(b)
	if err != nil {
		mr.Logger.Error("Failed to generate random bytes")
		return false
	}

	txs := &protomem.TestTx{StartTime: time.Now().Format(time.RFC3339Nano), Tx: b}
	return p2p.SendEnvelopeShim(peer, p2p.Envelope{
		Message:   txs,
		ChannelID: chID,
		Src:       peer,
	}, mr.Logger)
}

func (mr *MockReactor) FillChannel(id p2p.ID, chID byte, count, msgSize int) (bool, int, time.Duration) {
	start := time.Now()
	for i := 0; i < count; i++ {
		success := mr.SendBytes(id, chID, mr.size.Load())
		if !success {
			end := time.Now()
			return success, i, end.Sub(start)
		}
	}
	end := time.Now()
	return true, count, end.Sub(start)
}

func (mr *MockReactor) FloodChannel(id p2p.ID, d time.Duration, chIDs ...byte) {
	for _, chID := range chIDs {
		go func(d time.Duration, chID byte) {
			start := time.Now()
			for time.Since(start) < d {
				//time.Sleep(100 * time.Millisecond)
				success := mr.SendBytes(id, chID, mr.size.Load())
				if success {
					mr.metrics.mtx.Lock()
					if _, ok := mr.metrics.startUploadTime[string(id)]; !ok {
						mr.metrics.startUploadTime[string(id)] = time.Now()
					}
					mr.metrics.cumulativeUploadBytes[string(id)] += int(mr.size.Load())
					mr.metrics.uploadSpeed[string(id)] = float64(mr.metrics.cumulativeUploadBytes[string(id)]) / time.Now().Sub(mr.metrics.startUploadTime[string(id)]).Seconds()
					mr.metrics.mtx.Unlock()
				}
			}
		}(d, chID)
	}
}

func (mr *MockReactor) FloodAllPeers(d time.Duration, chIDs ...byte) {
	for _, peer := range mr.peers {
		mr.FloodChannel(peer.ID(), d, chIDs...)
	}
}

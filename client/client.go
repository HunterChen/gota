package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/jim3ma/gota/utils"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

type ConnManager struct {
	//mutex            *sync.Mutex
	nextConnID       uint16
	connChannel      <-chan *net.TCPConn
	connCloseChannel chan uint16
	x2cChannel       chan<- dataFrame
	c2xChannel       <-chan dataFrame
	connectionPool   map[uint16]*ConnHandler
}

func newConnManager(in <-chan *net.TCPConn, x2cChannel chan dataFrame, c2xChannel chan dataFrame) *ConnManager {
	closeChannel := make(chan uint16)
	connPool := make(map[uint16]*ConnHandler)
	c := &ConnManager{
		nextConnID:       1,
		connChannel:      in,
		connCloseChannel: closeChannel,
		x2cChannel:       x2cChannel,
		c2xChannel:       c2xChannel,
		connectionPool:   connPool,
	}
	//go c.handleConn()
	return c
}

// s2c connection for local port, and dispatch a Connection Handler to forward traffic
func (c *ConnManager) handleConn() {
	go c.closeConn()
	go c.dispatch()
	for conn := range c.connChannel {
		//t.mutex.Lock()
		c2xChannel := make(chan dataFrame)
		ch := ConnHandler{
			cid:              c.nextConnID,
			conn:             conn,
			connCloseChannel: c.connCloseChannel,
			x2cChannel:       c.x2cChannel,
			c2xChannel:       c2xChannel,
		}
		// all handlers share one c2s channel, and every handler uses one s2c channel,
		// we need register the s2c channel, so we can forward traffic from tunnels to local connection
		c.connectionPool[c.nextConnID] = &ch
		go ch.start()
		if c.nextConnID == MaxConnID {
			c.nextConnID = 1
		} else {
			c.nextConnID += 1
		}
		//t.mutex.Unlock()
	}
}

// c2s Magic number to server, than server will create a new connection
//func (c *ConnManager) createConnOnServer(cid uint16){
//	c.x2cChannel <-
//}

// s2c from c2xChannel and forward to special s2c channel according the connection ID
func (c *ConnManager) dispatch() {
	for d := range c.c2xChannel {
		log.Debugf("Received data from tunnel: %+v", d)
		if ch, ok := c.connectionPool[d.ConnId]; ok {
			ch.c2xChannel <- d
		} else {
			log.Errorf("Connection didn't exist, connection id: %d", d.ConnId)
		}
	}
}

func (c *ConnManager) closeConn() {
	for cid := range c.connCloseChannel {
		err := c.connectionPool[cid].conn.Close()
		if err != nil {
			log.Errorf("Close Connection Error: %s", err)
		}
		delete(c.connectionPool, cid)
	}
}

type ConnHandler struct {
	cid              uint16
	conn             *net.TCPConn
	connCloseChannel chan<- uint16
	x2cChannel       chan<- dataFrame
	c2xChannel       chan dataFrame
}

func (ch *ConnHandler) start() {
	// c2s Magic number to server, than server will create a peer connection
	req := dataFrame{
		ConnId: ch.cid,
		Length: 0,
		SeqNum: uint32(TMCreateConnSeq),
	}
	log.Debugf("Try to create a peer connection on server, connection id: %d", ch.cid)
	ch.x2cChannel <- req

	// wait for server response
	res := <-ch.c2xChannel
	if res.SeqNum != TMCreateConnOKSeq {
		log.Error("Create a peer connection failed, close client connection")
		//ch.connCloseChannel <- ch.cid
	}
	log.Debugf("Created a peer connection on server, connection id: %d", ch.cid)
	go ch.x2c()
	go ch.c2x()
}

func (ch *ConnHandler) x2c() {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Read data failed: %v", r)
			log.Errorf("Call stack: %s", debug.Stack())
		}
		ch.connCloseChannel <- ch.cid
	}()

	var seq uint32
	seq = 1
	for {
		data := make([]byte, MaxDataLength)
		n, err := ch.conn.Read(data)
		if n > 0 {
			df := dataFrame{
				ConnId: ch.cid,
				SeqNum: seq,
				Length: uint16(n),
				data:   data[:n],
			}
			seq += 1
			ch.x2cChannel <- df
		} else {
			log.Warn("Received empty data from x")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
	}
}

func (ch *ConnHandler) c2x() {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Write data failed: %s", r)
			log.Errorf("Call stack: %s", debug.Stack())
		}
	}()

	var seq uint32
	seq = 1
	cache := make(map[uint32][]byte)
	for d := range ch.c2xChannel {
		log.Debugf("Received from tunnel, data frame: %+v", d)
		if d.SeqNum == seq {
			_, err := ch.conn.Write(d.data)
			if err != nil && err != io.EOF {
				panic(err)
			}
			if err == io.EOF {
				return
			}
			seq += 1

			if len(cache) == 0 {
				continue
			}
			// TODO check cache and send to client
			for {
				if data, ok := cache[seq]; ok {
					_, err := ch.conn.Write(data)
					if err != nil && err != io.EOF {
						panic(err)
					}
					if err == io.EOF {
						return
					}
					delete(cache, seq)
					seq += 1
				} else {
					break
				}
			}
		} else if d.SeqNum > seq {
			// TODO cache for disorder data frame
			log.Debugf("Want to receive data frame seq: %d, but received seq: %d", seq, d.SeqNum)
			cache[d.SeqNum] = d.data
		}
	}
}

// connection pool
type TunnelManager struct {
	// Send Woker Pool
	//SWokerPool chan *TunnelWorker
	// Receive Worker pool
	//RWokerPool chan *TunnelWorker
	x2cChannel    <-chan dataFrame
	c2xChannel    chan<- dataFrame
	cancelChannel chan int
	localIPs      []string
	remoteAddrs   []string
	workerPool    []*TunnelWorker
	mode          int
}

func newTunnelManager(x2c chan dataFrame, c2x chan dataFrame, cancel chan int, mode int, localIPs []string, remoteAddrs []string) *TunnelManager {
	t := &TunnelManager{
		x2cChannel:    x2c,
		c2xChannel:    c2x,
		cancelChannel: cancel,
		localIPs:      localIPs,
		remoteAddrs:   remoteAddrs,
		mode:          mode,
	}
	//go t.start()
	return t
}

func (t *TunnelManager) start() {
	// TODO multi mode support
	switch t.mode {
	case TMConnBiuniqueMode:
		log.Info("Work Mode: Biunique")
	case TMConnOverlapMode:
		log.Info("Work Mode: Overlap")
	case TMConnMultiBiuniqueMode:
		log.Info("Work Mode: Multi Biunique")
	case TMConnMultiOverlapMode:
		log.Info("Work Mode: Multi Overlap")
	default:
		log.Error("Unknown Worker Mode")
		panic("Unknown Worker Mode")
	}
	for _, lAddr := range t.localIPs {
		for _, rAddr := range t.remoteAddrs {
			log.Infof("Local IP address: %s, remote address: %s", lAddr, rAddr)
			heartbeat := make(chan int)
			tw := &TunnelWorker{
				localAddr:     lAddr,
				remoteAddr:    rAddr,
				cancelChannel: t.cancelChannel,
				heartbeatChan: heartbeat,
				x2cChannel:    t.x2cChannel,
				c2xChannel:    t.c2xChannel,
			}
			go tw.start()
		}
	}
}

type TunnelWorker struct {
	//SWokerPool chan *TunnelWorker
	//RWokerPool chan *TunnelWorker
	localAddr     string
	remoteAddr    string
	cancelFlag    int
	cancelChannel chan int
	heartbeatChan chan int
	// read from client and c2s to server
	x2cChannel <-chan dataFrame
	// s2c from server and c2s to client
	c2xChannel chan<- dataFrame
	stat utils.Statistic
	conn *net.TCPConn
	//retryTime int
}

func (tw *TunnelWorker) heartbeat() {
	//log.Info("Start to send heartbeat to server")
	for {
		select {
		case <-time.After(time.Second * TMHeartBeatSecond):
			tw.heartbeatChan <- 0
			if tw.cancelFlag == -1 {
				log.Debug("Tunnel work was canneled")
				break
			}
		case <-time.After(time.Second * TMStatReportSecond):
			log.Infof("Traffic Report for client(%v) & server(%v): { sent: %d bytes, %d/second, received: %d bytes, %d/second }",
				tw.conn.LocalAddr(), tw.conn.RemoteAddr(),
				tw.stat.SentBytes, tw.stat.SendSpeed(), tw.stat.ReceivedBytes, tw.stat.ReceiveSpeed())
		}
	}
}

func (tw *TunnelWorker) start() {
	tw.stat = utils.Statistic{
		SentBytes: 0,
		ReceivedBytes: 0,
		StartSeconds: time.Now().Unix(),
	}
	// connect server
	lAddr, err := net.ResolveTCPAddr("tcp", tw.localAddr+":0")
	if err != nil {
		log.Errorf("Using Local Address: %s, Error: %s", lAddr, err)
		return
	}
	rAddr, err := net.ResolveTCPAddr("tcp", tw.remoteAddr)
	if err != nil {
		log.Errorf("Using Remote Address: %s, Error: %s", rAddr, err)
		return
	}

	conn, err := net.DialTCP("tcp", lAddr, rAddr)
	if err != nil {
		log.Errorf("Connect to Server: %s, using Local Address: %s, Error: %s", rAddr, lAddr, err)
		return
	}
	defer conn.Close()

	log.Debugf("Created a tunnel: %v to server: %v", conn.LocalAddr(), conn.RemoteAddr())

	c2sDone, s2cDone := make(chan int), make(chan int)
	go tw.heartbeat()
	go tw.c2s(c2sDone, conn)
	go tw.s2c(s2cDone, conn)
	log.Infof("TunnelWorker start to forward traffic, local address: %s, remote address: %s", tw.localAddr, tw.remoteAddr)
	<-c2sDone
	<-s2cDone
}

func (tw *TunnelWorker) resetart() {
	tw.cancelFlag = 0
	go tw.start()
}

// traffic -> gota client -> internet -> gota server -> ...
func (tw *TunnelWorker) c2s(done chan<- int, conn *net.TCPConn) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Runtime error caught: %v, runtime info: %s", r, utils.GoRuntimeInfo())
			log.Errorf("Call stack: %s", debug.Stack())
		}
		tw.cancelFlag = -1
		done <- 0
	}()

	log.Debugf("Tunnel work start to forward data from x client(%v) to server(%v)", conn.LocalAddr(), conn.RemoteAddr())
	for {
		select {
		case d := <-tw.x2cChannel:
			n, err := conn.Write(wrapDataFrame(d))
			if err != nil || n < 8 {
				panic(err)
			}
			tw.stat.AddSentBytes(uint64(n))
			log.Debugf("Wrote %d bytes", n)
			log.Debugf("Received data frame from x, send to server, data: %+v", d)
		case <-tw.cancelChannel:
			log.Infof("Shutdown Worker: %v", tw)
			_, err := conn.Write(TMCloseTunnelBytes)
			if err != nil {
				log.Fatalf("Send Close Signal failed duo to: %s", err)
			}
			//tw.cancelFlag = -1
			return
		case <-tw.heartbeatChan:
			_, err := conn.Write(TMHeartBeatBytes)
			log.Debugf("Sent heartbeat to server(%s) from client(%s)", conn.RemoteAddr(), conn.LocalAddr())
			if err != nil {
				log.Errorf("HeartBeat failed duo to: %s, stop this worker", err)
				//tw.cancelFlag = -1
				return
			}
		}
	}
}

// traffic <- gota client <- internet <- gota server <- ...
func (tw *TunnelWorker) s2c(done chan<- int, conn *net.TCPConn) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Runtime error caught: %v, runtime info: %s", r, utils.GoRuntimeInfo())
			log.Errorf("Call stack: %s", debug.Stack())
		}
		done <- 0
	}()
	log.Debugf("Tunnel work start to forward data from server(%v) to x client(%v)",
		conn.RemoteAddr(), conn.LocalAddr())
	for {
		header := make([]byte, 8)
		n, err := conn.Read(header)
		if n == 0 && err == nil {
			log.Debug("Receive empth data, skip and continue")
			continue
		} else if err != io.EOF && (err != nil || n != 8) {
			log.Error("Received data frame header error")
			//panic(err)
		}
		if err == io.EOF {
			log.Infof("Received data frame header io.EOF, stop this worker(client: %v, server: %v)",
				conn.LocalAddr(), conn.RemoteAddr())
			break
		}
		df := unWrapDataFrame(header)
		log.Debugf("Received data frame header from server: %+v", df)

		if df.Length == 0 {
			switch df.SeqNum {
			case TMHeartBeatSeq:
				log.Debugf("Received heartbeat signal from server(%s) to client(%s)", conn.RemoteAddr(), conn.LocalAddr())
				continue
			case TMCloseConnSeq:
				log.Debug("Received close connection signal")
				// TODO close connection
			case TMCreateConnSeq:
				log.Debug("Received create connection signal")
				log.Error("Create connection cignal only used in server")
			case TMCreateConnOKSeq:
				log.Debugf("Received create connection ok signal, connection id: %d", df.ConnId)
				tw.c2xChannel <- df
				continue
			case TMCloseTunnelSeq:
				log.Info("Receive close tunnel signal")
				// TODO close tunnel
			default:
				log.Errorf("Unkownn signal: %d", df.SeqNum)
				panic("Unkownn signal")
			}
		}

		data := make([]byte, MaxDataLength)
		n, err = conn.Read(data)
		// TODO partial data received!
		if (err != nil && err != io.EOF) || n != int(df.Length) {
			log.Errorf("Received mismatched length data, stop this worker(client: %v, server: %v)",
				conn.LocalAddr(), conn.RemoteAddr())
			break
		}
		df.data = data[:n]
		tw.stat.AddReceivedBytes(uint64(n))
		tw.c2xChannel <- df

		if tw.cancelFlag == -1 || err == io.EOF {
			// reset cancelFlag flag
			tw.cancelFlag = 0
			break
		}
	}
}

const MaxDataLength = 65536
const MaxConnID = 65535

// Connection Manage HeartBeat Time
const TMHeartBeatSecond = 300
const TMStatReportSecond = 30

// Magic data frame
// when length == 0 the data frame is used for control the tunnels and connections
//const TMHeartBeatString = "00000000"
//const TMHeartBeatSeq = 0

//const TMCloseConnString = "00000001"
//const TMCloseConnSeq = 1

//const TMCreateConnString = "00000002"
//const TMCreateConnSeq = 2

//const TMCloseTunnelString = "00000003"
//const TMCloseTunnelSeq = 3

const (
	TMHeartBeatSeq = iota
	TMCreateConnSeq
	TMCreateConnOKSeq
	TMCloseConnSeq
	TMCloseConnOKSeq
	TMCloseTunnelSeq
)

var TMHeartBeatBytes []byte
var TMCloseTunnelBytes []byte

func init() {
	TMHeartBeatBytes = wrapDataFrame(dataFrame{
		ConnId: uint16(0),
		Length: uint16(0),
		SeqNum: uint32(TMHeartBeatSeq),
	})
	TMCloseTunnelBytes = wrapDataFrame(dataFrame{
		ConnId: uint16(0),
		Length: uint16(0),
		SeqNum: uint32(TMCloseTunnelSeq),
	})
}

const (
	TMConnBiuniqueMode = iota
	TMConnOverlapMode
	TMConnMultiBiuniqueMode
	TMConnMultiOverlapMode
)

func wrapDataFrame(data dataFrame) []byte {
	var buf bytes.Buffer

	cid := make([]byte, 2)
	binary.LittleEndian.PutUint16(cid, data.ConnId)
	buf.Write(cid)

	lens := make([]byte, 2)
	binary.LittleEndian.PutUint16(lens, data.Length)
	buf.Write(lens)

	seq := make([]byte, 4)
	binary.LittleEndian.PutUint32(seq, data.SeqNum)
	buf.Write(seq)

	buf.Write(data.data)
	return buf.Bytes()
}

func unWrapDataFrame(h []byte) dataFrame {
	cid := binary.LittleEndian.Uint16(h[:2])
	lens := binary.LittleEndian.Uint16(h[2:4])
	seq := binary.LittleEndian.Uint32(h[4:])
	return dataFrame{
		ConnId: cid,
		Length: lens,
		SeqNum: seq,
	}
	/*
		cid, n := binary.Uvarint(h[:2])
		if n <= 0 {
			panic(fmt.Sprintf("Error Data Frame Header: %s", h))
		}

		lens, n := binary.Uvarint(h[2:4])
		if n <= 0 {
			panic(fmt.Sprintf("Error Data Frame Header: %s", h))
		}

		seq, n := binary.Uvarint(h[4:])
		if n <= 0 {
			panic(fmt.Sprintf("Error Data Frame Header: %s", h))
		}
		return dataFrame{
			uint16(cid),
			uint16(lens),
			uint32(seq),
			nil}
	*/
}

type dataFrame struct {
	// Connection ID
	ConnId uint16
	// Data length
	Length uint16
	// Sequence number
	SeqNum uint32
	data   []byte
}

func main() {
	// pprof debug
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Call stack: %s", debug.Stack())
			log.Fatalf("Runtime error caught: %v, runtime info: %s", r, utils.GoRuntimeInfo())
		}
	}()

	// TODO configuration
	log.SetLevel(log.DebugLevel)
	localAddr := "localhost:8081"
	//remoteAddr := "localhost:1081"

	addr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		panic(err)
	}

	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		panic(err)
	}

	newConnChannel := make(chan *net.TCPConn)

	x2cChannel, c2xChannel := make(chan dataFrame), make(chan dataFrame)
	cm := newConnManager(newConnChannel, x2cChannel, c2xChannel)
	go cm.handleConn()

	cancelChannel := make(chan int)
	tm := newTunnelManager(x2cChannel, c2xChannel, cancelChannel, TMConnBiuniqueMode, []string{"127.0.0.1", "127.0.0.1"}, []string{"127.0.0.1:8080", "127.0.0.1:8080"})
	go tm.start()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs
		fmt.Println(sig)
		os.Exit(1)
	}()

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			//panic(err)
			// TODO error handle which accept new connection
			log.Errorf("Accept Connection Error: %s", err)
			continue
		}
		log.Debugf("Received new connection from %s", conn.RemoteAddr())
		newConnChannel <- conn
	}
}

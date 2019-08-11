package tun2socks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/FlowerWrong/netstack/tcpip"
	"github.com/FlowerWrong/netstack/waiter"
	"github.com/FlowerWrong/tun2socks/util"
)

// TCPTunnel struct
type TCPTunnel struct {
	wq                   *waiter.Queue
	localEndpoint        tcpip.Endpoint
	localEndpointStatus  TunnelStatus // to avoid panic: send on closed channel
	localEndpointRwMutex sync.RWMutex
	remoteConn           net.Conn
	remoteAddr           string
	remoteStatus         TunnelStatus // to avoid panic: send on closed channel
	remoteRwMutex        sync.RWMutex
	ctx                  context.Context
	ctxCancel            context.CancelFunc
	closeOne             sync.Once // to avoid multi close tunnel
	app                  *App
}

// NewTCP2Socks create a tcp tunnel
func NewTCP2Socks(wq *waiter.Queue, ep tcpip.Endpoint, ip net.IP, port uint16, app *App) (*TCPTunnel, error) {
	var remoteAddr string
	proxy := ""

	if app.FakeDNS != nil {
		record := app.FakeDNS.DNSTablePtr.GetByIP(ip)
		if record != nil {
			if record.Proxy == "block" {
				return nil, errors.New(record.Hostname + " is blocked")
			}

			remoteAddr = fmt.Sprintf("%v:%d", record.Hostname, port)
			proxy = record.Proxy
		} else {
			remoteAddr = fmt.Sprintf("%v:%d", ip, port)
		}
	} else {
		remoteAddr = fmt.Sprintf("%v:%d", ip, port)
	}

	socks5Conn, err := app.Proxies.Dial(proxy, remoteAddr)
	if err != nil {
		log.Printf("[tcp] dial %s by proxy %q failed: %s", remoteAddr, proxy, err)
		return nil, err
	}
	socks5Conn.(*net.TCPConn).SetKeepAlive(true)
	socks5Conn.SetDeadline(WithoutTimeout)

	return &TCPTunnel{
		wq:                   wq,
		localEndpoint:        ep,
		remoteConn:           socks5Conn,
		remoteAddr:           remoteAddr,
		localEndpointRwMutex: sync.RWMutex{},
		remoteRwMutex:        sync.RWMutex{},
		app:                  app,
	}, nil
}

// SetRemoteStatus with rwMutex
func (tcpTunnel *TCPTunnel) SetRemoteStatus(s TunnelStatus) {
	tcpTunnel.remoteRwMutex.Lock()
	tcpTunnel.remoteStatus = s
	tcpTunnel.remoteRwMutex.Unlock()
}

// RemoteStatus with rwMutex
func (tcpTunnel *TCPTunnel) RemoteStatus() TunnelStatus {
	tcpTunnel.remoteRwMutex.RLock()
	s := tcpTunnel.remoteStatus
	tcpTunnel.remoteRwMutex.RUnlock()
	return s
}

// SetLocalEndpointStatus with rwMutex
func (tcpTunnel *TCPTunnel) SetLocalEndpointStatus(s TunnelStatus) {
	tcpTunnel.localEndpointRwMutex.Lock()
	tcpTunnel.localEndpointStatus = s
	tcpTunnel.localEndpointRwMutex.Unlock()
}

// LocalEndpointStatus with rwMutex
func (tcpTunnel *TCPTunnel) LocalEndpointStatus() TunnelStatus {
	tcpTunnel.localEndpointRwMutex.RLock()
	s := tcpTunnel.localEndpointStatus
	tcpTunnel.localEndpointRwMutex.RUnlock()
	return s
}

// Run start tcp tunnel
func (tcpTunnel *TCPTunnel) Run() {
	tcpTunnel.ctx, tcpTunnel.ctxCancel = context.WithCancel(context.Background())
	wgw := new(util.WaitGroupWrapper)
	wgw.Wrap(func() {
		tcpTunnel.readFromRemoteWriteToLocal()
	})
	wgw.Wrap(func() {
		tcpTunnel.readFromLocalWriteToRemote()
	})

	tcpTunnel.SetRemoteStatus(StatusProxying)
	tcpTunnel.SetLocalEndpointStatus(StatusProxying)

	wgw.WaitGroup.Wait()
	tcpTunnel.Close(nil)
}

func (tcpTunnel *TCPTunnel) readFromLocalWriteToRemote() {
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	tcpTunnel.wq.EventRegister(&waitEntry, waiter.EventIn)
	defer tcpTunnel.wq.EventUnregister(&waitEntry)

readFromLocal:
	for {
		select {
		case <-tcpTunnel.ctx.Done():
			break readFromLocal
		default:
			v, _, err := tcpTunnel.localEndpoint.Read(nil)
			if err != nil {
				if err == tcpip.ErrWouldBlock {
					select {
					case <-tcpTunnel.ctx.Done():
						break readFromLocal
					case <-notifyCh:
						continue readFromLocal
					}
				}
				if err == tcpip.ErrClosedForSend || err == tcpip.ErrClosedForReceive {
					// do nothing
				} else if util.IsClosed(err) {
					tcpTunnel.Close(nil)
				} else {
					log.Println(err)
					tcpTunnel.Close(errors.New("read from local failed, " + err.String()))
				}
				break readFromLocal
			}
			if tcpTunnel.LocalEndpointStatus() != StatusClosed {

			writeAllPacket:
				for {
					if len(v) <= 0 {
						break writeAllPacket
					}
					n, err := tcpTunnel.remoteConn.Write(v)
					if err != nil {
						if util.IsBrokenPipe(err) || util.IsEOF(err) {
							tcpTunnel.Close(nil)
						} else {
							log.Println(err)
							tcpTunnel.Close(err)
						}
						break readFromLocal
					} else if n < len(v) {
						v = v[n:]
						continue writeAllPacket
					} else {
						break writeAllPacket
					}
				}
			} else {
				break readFromLocal
			}
		}
	}
}

func (tcpTunnel *TCPTunnel) readFromRemoteWriteToLocal() {
readFromRemote:
	for {
		select {
		case <-tcpTunnel.ctx.Done():
			break readFromRemote
		default:
			buf := make([]byte, BuffSize)
			tcpTunnel.remoteConn.SetReadDeadline(time.Now().Add(time.Duration(tcpTunnel.app.Cfg.TCP.Timeout) * time.Second))
			n, err := tcpTunnel.remoteConn.Read(buf)
			if err != nil {
				if !util.IsTimeout(err) && !util.IsConnectionReset(err) && !util.IsEOF(err) {
					log.Println(err)
					tcpTunnel.Close(err)
				} else {
					tcpTunnel.Close(nil)
				}
				break readFromRemote
			}
			tcpTunnel.remoteConn.SetReadDeadline(WithoutTimeout)

			if n > 0 && tcpTunnel.RemoteStatus() != StatusClosed {
				chunk := buf[0:n]
			writeAllPacket:
				for {
					if len(chunk) <= 0 {
						break writeAllPacket
					}
					var m uintptr
					var err *tcpip.Error
					m, _, err = tcpTunnel.localEndpoint.Write(tcpip.SlicePayload(chunk), tcpip.WriteOptions{})
					n := int(m)
					if err != nil {
						if err == tcpip.ErrWouldBlock {
							if n < len(chunk) {
								chunk = chunk[n:]
								continue writeAllPacket
							}
						}
						if err == tcpip.ErrClosedForSend || err == tcpip.ErrClosedForReceive {
							// do nothing
						} else if util.IsClosed(err) {
							tcpTunnel.Close(nil)
						} else {
							log.Println(err)
							tcpTunnel.Close(errors.New(err.String()))
						}
						break readFromRemote
					} else if n < len(chunk) {
						chunk = chunk[n:]
						continue writeAllPacket
					} else {
						break writeAllPacket
					}
				}
			} else {
				break readFromRemote
			}
		}
	}
}

// Close this tcp tunnel
func (tcpTunnel *TCPTunnel) Close(reason error) {
	tcpTunnel.closeOne.Do(func() {
		if reason != nil {
			local, _ := tcpTunnel.localEndpoint.GetLocalAddress()
			ip := net.ParseIP(local.Addr.To4().String())
			log.Println("tcp tunnel closed reason:", reason.Error(), tcpTunnel.remoteAddr, fmt.Sprintf("%v:%d", ip, local.Port), tcpTunnel.remoteConn.LocalAddr(), tcpTunnel.remoteConn.RemoteAddr())
		}
		tcpTunnel.SetLocalEndpointStatus(StatusClosed)
		tcpTunnel.SetRemoteStatus(StatusClosed)

		tcpTunnel.ctxCancel()

		tcpTunnel.localEndpoint.Close()
		tcpTunnel.remoteConn.Close()
	})
}

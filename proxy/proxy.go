// Package proxy contains all proxies used by Xray.
//
// To implement an inbound or outbound proxy, one needs to do the following:
// 1. Implement the interface(s) below.
// 2. Register a config creator through common.RegisterConfig.
package proxy

import (
	"context"
	gotls "crypto/tls"
	"io"
	"runtime"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

// An Inbound processes inbound connections.
type Inbound interface {
	// Network returns a list of networks that this inbound supports. Connections with not-supported networks will not be passed into Process().
	Network() []net.Network

	// Process processes a connection of given network. If necessary, the Inbound can dispatch the connection to an Outbound.
	Process(context.Context, net.Network, stat.Connection, routing.Dispatcher) error
}

// An Outbound process outbound connections.
type Outbound interface {
	// Process processes the given connection. The given dialer may be used to dial a system outbound connection.
	Process(context.Context, *transport.Link, internet.Dialer) error
}

// UserManager is the interface for Inbounds and Outbounds that can manage their users.
type UserManager interface {
	// AddUser adds a new user.
	AddUser(context.Context, *protocol.MemoryUser) error

	// RemoveUser removes a user by email.
	RemoveUser(context.Context, string) error
}

type GetInbound interface {
	GetInbound() Inbound
}

type GetOutbound interface {
	GetOutbound() Outbound
}

// CopyRawConnIfExist use the most efficient copy method.
// - If caller don't want to turn on splice, do not pass in both reader conn and writer conn
// - reader and writer are from *transport.Link, one of them must be nil (idicate the direction of copy)
func CopyRawConnIfExist(ctx context.Context, readerConn net.Conn, writerConn net.Conn, reader buf.Reader, writer buf.Writer, timer signal.ActivityUpdater) error {
	var readCounter stats.Counter
	if readerConn != nil {
		statConn, ok := readerConn.(*stat.CounterConnection)
		if ok {
			readerConn = statConn.Connection
			readCounter = statConn.ReadCounter
		}
		if xc, ok := readerConn.(*gotls.Conn); ok {
			readerConn = xc.NetConn()
		} else if utlsConn, ok := readerConn.(*tls.UConn); ok {
			readerConn = utlsConn.NetConn()
		} else if realityConn, ok := readerConn.(*reality.Conn); ok {
			readerConn = realityConn.NetConn()
		}
	}
	var writeCounter stats.Counter
	if writerConn != nil {
		statConn, ok := writerConn.(*stat.CounterConnection)
		if ok {
			writerConn = statConn.Connection
			writeCounter = statConn.WriteCounter
		}
		if xc, ok := writerConn.(*gotls.Conn); ok {
			writerConn = xc.NetConn()
		} else if utlsConn, ok := writerConn.(*tls.UConn); ok {
			writerConn = utlsConn.NetConn()
		} else if realityConn, ok := writerConn.(*reality.Conn); ok {
			writerConn = realityConn.NetConn()
		}
	}

	if tc, ok := writerConn.(*net.TCPConn); ok && readerConn != nil && writerConn != nil && (runtime.GOOS == "linux" || runtime.GOOS == "android") {
		newError("CopyRawConn splice").WriteToLog(session.ExportIDToError(ctx))
		runtime.Gosched() // necessary
		w, err := tc.ReadFrom(readerConn)
		if readCounter != nil {
			readCounter.Add(w)
		}
		if writeCounter != nil {
			writeCounter.Add(w)
		}
		if err != nil && errors.Cause(err) != io.EOF {
			return err
		}
		return nil
	}
	if reader == nil {
		newError("CopyRawConn copy from readerConn to *transport.Link.Writer").WriteToLog(session.ExportIDToError(ctx))
		reader = buf.NewReader(readerConn)
		writeCounter = nil
	}
	if writer == nil {
		newError("CopyRawConn copy from *transport.Link.Reader to writerConn").WriteToLog(session.ExportIDToError(ctx))
		writer = buf.NewWriter(writerConn)
		readCounter = nil
	}
	if err := buf.Copy(reader, writer, buf.UpdateActivity(timer), buf.AddToStatCounter(readCounter), buf.AddToStatCounter(writeCounter)); err != nil {
		return newError("failed to process response").Base(err)
	}
	return nil
}

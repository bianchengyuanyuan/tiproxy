// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"crypto/tls"
	"net"

	"github.com/pingcap/TiProxy/pkg/manager/namespace"
	"github.com/pingcap/TiProxy/pkg/proxy/backend"
	pnet "github.com/pingcap/TiProxy/pkg/proxy/net"
	"github.com/pingcap/TiProxy/pkg/util/errors"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

type ClientConnection struct {
	logger           *zap.Logger
	serverTLSConfig  *tls.Config    // the TLS config to connect to clients.
	backendTLSConfig *tls.Config    // the TLS config to connect to TiDB server.
	pkt              *pnet.PacketIO // a helper to read and write data in packet format.
	nsmgr            *namespace.NamespaceManager
	ns               *namespace.Namespace
	connMgr          *backend.BackendConnManager
}

func NewClientConnection(logger *zap.Logger, conn net.Conn, serverTLSConfig *tls.Config, backendTLSConfig *tls.Config, nsmgr *namespace.NamespaceManager, bemgr *backend.BackendConnManager) *ClientConnection {
	pkt := pnet.NewPacketIO(conn)
	return &ClientConnection{
		logger:           logger,
		serverTLSConfig:  serverTLSConfig,
		backendTLSConfig: backendTLSConfig,
		pkt:              pkt,
		nsmgr:            nsmgr,
		connMgr:          bemgr,
	}
}

func (cc *ClientConnection) Addr() string {
	return cc.pkt.RemoteAddr().String()
}

func (cc *ClientConnection) connectBackend(ctx context.Context) error {
	ns, ok := cc.nsmgr.GetNamespace("")
	if !ok {
		return errors.New("failed to find a namespace")
	}
	cc.ns = ns
	router := ns.GetRouter()
	addr, err := router.Route(cc.connMgr)
	if err != nil {
		return err
	}
	if err = cc.connMgr.Connect(ctx, addr, cc.pkt, cc.serverTLSConfig, cc.backendTLSConfig); err != nil {
		return err
	}
	return nil
}

func (cc *ClientConnection) Run(ctx context.Context) {
	if err := cc.connectBackend(ctx); err != nil {
		logutil.Logger(ctx).Info("new connection fails", zap.String("remoteAddr", cc.Addr()), zap.Error(err))
		metrics.HandShakeErrorCounter.Inc()
		return
	}

	if err := cc.processMsg(ctx); err != nil {
		cc.logger.Info("process message fails", zap.String("remoteAddr", cc.Addr()), zap.Error(err))
	}
}

func (cc *ClientConnection) processMsg(ctx context.Context) error {
	for {
		cc.pkt.ResetSequence()
		clientPkt, err := cc.pkt.ReadPacket()
		if err != nil {
			return err
		}
		err = cc.connMgr.ExecuteCmd(ctx, clientPkt, cc.pkt)
		if err != nil {
			return err
		}
		cmd := clientPkt[0]
		switch cmd {
		case mysql.ComQuit:
			return nil
		}
	}
}

func (cc *ClientConnection) Close() error {
	var errs []error
	if err := cc.pkt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := cc.connMgr.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Collect(ErrCloseConn, errs...)
}
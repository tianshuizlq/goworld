package main

import (
	"fmt"
	"time"

	"net"

	"sync"

	"os"

	"github.com/xiaonanln/go-xnsyncutil/xnsyncutil"
	"github.com/xiaonanln/goworld/common"
	"github.com/xiaonanln/goworld/components/dispatcher/dispatcher_client"
	"github.com/xiaonanln/goworld/config"
	"github.com/xiaonanln/goworld/consts"
	"github.com/xiaonanln/goworld/gwlog"
	"github.com/xiaonanln/goworld/netutil"
	"github.com/xiaonanln/goworld/opmon"
	"github.com/xiaonanln/goworld/proto"
)

type GateService struct {
	listenAddr        string
	clientProxies     map[common.ClientID]*ClientProxy
	clientProxiesLock sync.RWMutex
	packetQueue       *xnsyncutil.SyncQueue

	filterTreesLock sync.Mutex
	filterTrees     map[string]*FilterTree

	pendingSyncPackets     []*netutil.Packet
	pendingSyncPacketsLock sync.Mutex

	terminating xnsyncutil.AtomicBool
	terminated  *xnsyncutil.OneTimeCond
}

func newGateService() *GateService {
	return &GateService{
		//packetQueue: make(chan packetQueueItem, consts.DISPATCHER_CLIENT_PACKET_QUEUE_SIZE),
		clientProxies:      map[common.ClientID]*ClientProxy{},
		packetQueue:        xnsyncutil.NewSyncQueue(),
		filterTrees:        map[string]*FilterTree{},
		pendingSyncPackets: []*netutil.Packet{},
		terminated:         xnsyncutil.NewOneTimeCond(),
	}
}

func (gs *GateService) run() {
	cfg := config.GetGate(gateid)
	gwlog.Info("Compress connection: %v", cfg.CompressConnection)
	gs.listenAddr = fmt.Sprintf("%s:%d", cfg.Ip, cfg.Port)
	go netutil.ServeForever(gs.handlePacketRoutine)
	netutil.ServeTCPForever(gs.listenAddr, gs)
}

func (gs *GateService) String() string {
	return fmt.Sprintf("GateService<%s>", gs.listenAddr)
}

func (gs *GateService) ServeTCPConnection(conn net.Conn) {
	if gs.terminating.Load() {
		// server terminating, not accepting more connections
		conn.Close()
		return
	}

	cfg := config.GetGate(gateid)
	cp := newClientProxy(conn, cfg)

	gs.clientProxiesLock.Lock()
	gs.clientProxies[cp.clientid] = cp
	gs.clientProxiesLock.Unlock()

	dispatcher_client.GetDispatcherClientForSend().SendNotifyClientConnected(cp.clientid)
	if consts.DEBUG_CLIENTS {
		gwlog.Debug("%s.ServeTCPConnection: client %s connected", gs, cp)
	}
	cp.serve()
}

func (gs *GateService) onClientProxyClose(cp *ClientProxy) {
	gs.clientProxiesLock.Lock()
	delete(gs.clientProxies, cp.clientid)
	gs.clientProxiesLock.Unlock()

	gs.filterTreesLock.Lock()
	for key, val := range cp.filterProps {
		ft := gs.filterTrees[key]
		if ft != nil {
			if consts.DEBUG_FILTER_PROP {
				gwlog.Debug("DROP CLIENT %s FILTER PROP: %s = %s", cp, key, val)
			}
			ft.Remove(cp.clientid, val)
		}
	}
	gs.filterTreesLock.Unlock()

	dispatcher_client.GetDispatcherClientForSend().SendNotifyClientDisconnected(cp.clientid)
	if consts.DEBUG_CLIENTS {
		gwlog.Debug("%s.onClientProxyClose: client %s disconnected", gs, cp)
	}
}

func (gs *GateService) HandleDispatcherClientPacket(msgtype proto.MsgType_t, packet *netutil.Packet) {
	if consts.DEBUG_PACKETS {
		gwlog.Debug("%s.HandleDispatcherClientPacket: msgtype=%v, packet(%d)=%v", gs, msgtype, packet.GetPayloadLen(), packet.Payload())
	}

	if msgtype >= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_START && msgtype <= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_STOP {
		_ = packet.ReadUint16() // gid
		clientid := packet.ReadClientID()

		gs.clientProxiesLock.RLock()
		clientproxy := gs.clientProxies[clientid]
		gs.clientProxiesLock.RUnlock()

		if clientproxy != nil {
			if msgtype == proto.MT_SET_CLIENTPROXY_FILTER_PROP {
				gs.handleSetClientFilterProp(clientproxy, packet)
			} else if msgtype == proto.MT_CLEAR_CLIENTPROXY_FILTER_PROPS {
				gs.handleClearClientFilterProps(clientproxy, packet)
			} else {
				// message types that should be redirected to client proxy
				clientproxy.SendPacket(packet)
			}
		} else {
			// client already disconnected, but the game service seems not knowing it, so tell it
			dispatcher_client.GetDispatcherClientForSend().SendNotifyClientDisconnected(clientid)
		}
	} else if msgtype == proto.MT_SYNC_POSITION_YAW_ON_CLIENTS {
		gs.handleSyncPositionYawOnClients(packet)
	} else if msgtype == proto.MT_CALL_FILTERED_CLIENTS {
		gs.handleCallFilteredClientProxies(packet)
	} else {
		gwlog.Panicf("%s: unknown msg type: %d", gs, msgtype)
		if consts.DEBUG_MODE {
			os.Exit(2)
		}
	}
}

func (gs *GateService) handleSetClientFilterProp(clientproxy *ClientProxy, packet *netutil.Packet) {
	gwlog.Debug("%s.handleSetClientFilterProp: clientproxy=%s", gs, clientproxy)
	key := packet.ReadVarStr()
	val := packet.ReadVarStr()
	clientid := clientproxy.clientid

	gs.filterTreesLock.Lock()
	ft, ok := gs.filterTrees[key]
	if !ok {
		ft = NewFilterTree()
		gs.filterTrees[key] = ft
	}

	oldVal, ok := clientproxy.filterProps[key]
	if ok {
		if consts.DEBUG_FILTER_PROP {
			gwlog.Debug("REMOVE CLIENT %s FILTER PROP: %s = %s", clientproxy, key, val)
		}
		ft.Remove(clientid, oldVal)
	}
	clientproxy.filterProps[key] = val
	ft.Insert(clientid, val)
	gs.filterTreesLock.Unlock()

	if consts.DEBUG_FILTER_PROP {
		gwlog.Debug("SET CLIENT %s FILTER PROP: %s = %s", clientproxy, key, val)
	}
}

func (gs *GateService) handleClearClientFilterProps(clientproxy *ClientProxy, packet *netutil.Packet) {
	gwlog.Debug("%s.handleClearClientFilterProps: clientproxy=%s", gs, clientproxy)
	clientid := clientproxy.clientid

	gs.filterTreesLock.Lock()

	for key, val := range clientproxy.filterProps {
		ft, ok := gs.filterTrees[key]
		if !ok {
			continue
		}
		ft.Remove(clientid, val)
	}
	gs.filterTreesLock.Unlock()

	if consts.DEBUG_FILTER_PROP {
		gwlog.Debug("CLEAR CLIENT %s FILTER PROPS", clientproxy)
	}
}

func (gs *GateService) handleSyncPositionYawOnClients(packet *netutil.Packet) {
	_ = packet.ReadUint16() // read useless gateid
	payload := packet.UnreadPayload()
	payloadLen := len(payload)
	dispatch := map[common.ClientID][]byte{}
	for i := 0; i < payloadLen; i += common.CLIENTID_LENGTH + common.ENTITYID_LENGTH + proto.SYNC_INFO_SIZE_PER_ENTITY {
		clientid := common.ClientID(payload[i : i+common.CLIENTID_LENGTH])
		data := payload[i+common.CLIENTID_LENGTH : i+common.CLIENTID_LENGTH+common.ENTITYID_LENGTH+proto.SYNC_INFO_SIZE_PER_ENTITY]
		dispatch[clientid] = append(dispatch[clientid], data...)
	}

	// multiple entity sync infos are received from game->dispatcher, gate need to dispatcher these infos to different clients
	gs.clientProxiesLock.RLock()

	for clientid, data := range dispatch {
		clientproxy := gs.clientProxies[clientid]
		if clientproxy != nil {
			packet := netutil.NewPacket()
			packet.AppendUint16(proto.MT_SYNC_POSITION_YAW_ON_CLIENTS)
			packet.AppendBytes(data)
			packet.SetNotCompress() // too many these packets, giveup compress to save time
			clientproxy.SendPacket(packet)
			packet.Release()
		}
	}

	gs.clientProxiesLock.RUnlock()
}

func (gs *GateService) handleCallFilteredClientProxies(packet *netutil.Packet) {
	key := packet.ReadVarStr()
	val := packet.ReadVarStr()

	gs.filterTreesLock.Lock()
	gs.clientProxiesLock.RLock()

	ft := gs.filterTrees[key]
	if ft != nil {
		ft.Visit(val, func(clientid common.ClientID) {
			//// visit all clientids and
			clientproxy := gs.clientProxies[clientid]
			if clientproxy != nil {
				clientproxy.SendPacket(packet)
			}
		})
	}

	gs.clientProxiesLock.RUnlock()
	gs.filterTreesLock.Unlock()
}

func (gs *GateService) handleSyncPositionYawFromClient(packet *netutil.Packet) {
	packet.AddRefCount(1)
	gs.pendingSyncPacketsLock.Lock()
	gs.pendingSyncPackets = append(gs.pendingSyncPackets, packet)
	gs.pendingSyncPacketsLock.Unlock()
	//eid := packet.ReadEntityID()
	//x := packet.ReadFloat32()
	//y := packet.ReadFloat32()
	//z := packet.ReadFloat32()
	//yaw := packet.ReadFloat32()
}

func (gs *GateService) handleDispatcherClientBeforeFlush() {
	gs.pendingSyncPacketsLock.Lock()
	pendingSyncPackets := gs.pendingSyncPackets
	gs.pendingSyncPackets = make([]*netutil.Packet, 0, len(pendingSyncPackets))
	gs.pendingSyncPacketsLock.Unlock()
	// merge all client sync packets, and send in one packet (to reduce dispatcher overhead)

	if len(pendingSyncPackets) == 0 {
		return
	}

	packet := pendingSyncPackets[0] // use the first packet for sending
	if len(packet.UnreadPayload()) != common.ENTITYID_LENGTH+proto.SYNC_INFO_SIZE_PER_ENTITY {
		gwlog.Panicf("%s.handleDispatcherClientBeforeFlush: entity sync info size should be %d, but received %d", gs, proto.SYNC_INFO_SIZE_PER_ENTITY, len(packet.UnreadPayload())-common.ENTITYID_LENGTH)
	}

	//gwlog.Info("sycn packet payload len %d, unread %d", packet.GetPayloadLen(), len(packet.UnreadPayload()))
	for _, syncPkt := range pendingSyncPackets[1:] { // merge other packets to the first packet
		//gwlog.Info("sycn packet unread %d", len(syncPkt.UnreadPayload()))
		packet.AppendBytes(syncPkt.UnreadPayload())
		syncPkt.Release()
	}
	dispatcher_client.GetDispatcherClientForSend().SendPacket(packet)
	packet.Release()
}

type packetQueueItem struct { // packet queue from dispatcher client
	msgtype proto.MsgType_t
	packet  *netutil.Packet
}

func (gs *GateService) handlePacketRoutine() {
	for {
		item := gs.packetQueue.Pop().(packetQueueItem)
		op := opmon.StartOperation("GateServiceHandlePacket")
		gs.HandleDispatcherClientPacket(item.msgtype, item.packet)
		op.Finish(time.Millisecond * 100)
		item.packet.Release()
	}
}

func (gs *GateService) terminate() {
	gs.terminating.Store(true)

	gs.clientProxiesLock.RLock()

	for _, cp := range gs.clientProxies { // close all connected clients when terminating
		cp.Close()
	}

	gs.clientProxiesLock.RUnlock()

	gs.terminated.Signal()
}

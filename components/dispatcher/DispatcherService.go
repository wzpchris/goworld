package main

import (
	"fmt"

	"net"

	"time"

	"os"

	"math/rand"

	"github.com/pkg/errors"
	"github.com/xiaonanln/goworld/engine/binutil"
	"github.com/xiaonanln/goworld/engine/common"
	"github.com/xiaonanln/goworld/engine/config"
	"github.com/xiaonanln/goworld/engine/consts"
	"github.com/xiaonanln/goworld/engine/entity"
	"github.com/xiaonanln/goworld/engine/gwlog"
	"github.com/xiaonanln/goworld/engine/gwutils"
	"github.com/xiaonanln/goworld/engine/netutil"
	"github.com/xiaonanln/goworld/engine/post"
	"github.com/xiaonanln/goworld/engine/proto"
)

type callQueueItem struct {
	packet *netutil.Packet
}

type entityDispatchInfo struct {
	gameid             uint16
	blockUntilTime     time.Time
	pendingPacketQueue []*netutil.Packet
}

func newEntityDispatchInfo() *entityDispatchInfo {
	return &entityDispatchInfo{}
}

func (edi *entityDispatchInfo) blockRPC(d time.Duration) {
	t := time.Now().Add(d)
	if edi.blockUntilTime.Before(t) {
		edi.blockUntilTime = t
	}
}

func (edi *entityDispatchInfo) dispatchPacket(pkt *netutil.Packet) error {
	if edi.blockUntilTime.IsZero() {
		// most common case. handle it quickly
		return dispatcherService.dispatchPacketToGame(edi.gameid, pkt)
	}

	// blockUntilTime is set, need to check if block should be released
	now := time.Now()
	if now.Before(edi.blockUntilTime) {
		// keep blocking, just put the call to wait
		if len(edi.pendingPacketQueue) < consts.ENTITY_PENDING_PACKET_QUEUE_MAX_LEN {
			pkt.AddRefCount(1)
			edi.pendingPacketQueue = append(edi.pendingPacketQueue, pkt)
			return nil
		} else {
			gwlog.Errorf("%s.dispatchPacket: packet queue too long, packet dropped", edi)
			return errors.Errorf("%s: packet of entity %s is dropped", dispatcherService)
		}
	} else {
		// time to unblock
		edi.unblock()
		return nil
	}
}

func (info *entityDispatchInfo) unblock() {
	if !info.blockUntilTime.IsZero() { // entity is loading, it's done now
		//gwlog.Infof("entity is loaded now, clear loadTime")
		info.blockUntilTime = time.Time{}

		targetGame := info.gameid
		// send the cached calls to target game
		var pendingPackets []*netutil.Packet
		pendingPackets, info.pendingPacketQueue = info.pendingPacketQueue, nil
		for _, pkt := range pendingPackets {
			dispatcherService.dispatchPacketToGame(targetGame, pkt)
			pkt.Release()
		}
	}
}

type gameDispatchInfo struct {
	gameid             uint16
	clientProxy        *dispatcherClientProxy
	isBlocked          bool
	blockUntilTime     time.Time // game can be blocked
	pendingPacketQueue []*netutil.Packet
	isBanBootEntity    bool
}

func (gdi *gameDispatchInfo) setClientProxy(clientProxy *dispatcherClientProxy) {
	gdi.clientProxy = clientProxy
	if gdi.clientProxy != nil && !gdi.isBlocked {
		gdi.sendPendingPackets()
	}
	return
}

func (gdi *gameDispatchInfo) block(duration time.Duration) {
	gdi.isBlocked = true
	gdi.blockUntilTime = time.Now().Add(duration)
}

func (gdi *gameDispatchInfo) checkBlocked() bool {
	if gdi.isBlocked {
		if time.Now().After(gdi.blockUntilTime) {
			gdi.isBlocked = false
			return true
		}
	}
	return false
}

func (gdi *gameDispatchInfo) isConnected() bool {
	return gdi.clientProxy != nil
}

func (gdi *gameDispatchInfo) dispatchPacket(pkt *netutil.Packet) error {
	if gdi.checkBlocked() && gdi.clientProxy == nil {
		// blocked from true -> false, and game is already disconnected before
		// in this case, the game should be cleaned up
		dispatcherService.cleanupGameInfo(gdi.gameid, gdi)
	}

	if !gdi.isBlocked && gdi.clientProxy != nil {
		return gdi.clientProxy.SendPacket(pkt)
	} else {
		if len(gdi.pendingPacketQueue) < consts.GAME_PENDING_PACKET_QUEUE_MAX_LEN {
			gdi.pendingPacketQueue = append(gdi.pendingPacketQueue, pkt)
			pkt.AddRefCount(1)

			if len(gdi.pendingPacketQueue)%1 == 0 {
				gwlog.Warnf("game %d pending packet count = %d, blocked = %v, clientProxy = %s", gdi.gameid, len(gdi.pendingPacketQueue), gdi.isBlocked, gdi.clientProxy)
			}
			return nil
		} else {
			return errors.Errorf("packet to game %d is dropped", gdi.gameid)
		}
	}
}

func (gdi *gameDispatchInfo) unblock() {
	if !gdi.blockUntilTime.IsZero() {
		gdi.blockUntilTime = time.Time{}

		if gdi.clientProxy != nil {
			gdi.sendPendingPackets()
		}
	}
}

func (gdi *gameDispatchInfo) sendPendingPackets() {
	// send the cached calls to target game
	var pendingPackets []*netutil.Packet
	pendingPackets, gdi.pendingPacketQueue = gdi.pendingPacketQueue, nil
	for _, pkt := range pendingPackets {
		gdi.clientProxy.SendPacket(pkt)
		pkt.Release()
	}
}

func (gdi *gameDispatchInfo) clearPendingPackets() {
	var pendingPackets []*netutil.Packet
	pendingPackets, gdi.pendingPacketQueue = gdi.pendingPacketQueue, nil
	for _, pkt := range pendingPackets {
		pkt.Release()
	}
}

type dispatcherMessage struct {
	dcp *dispatcherClientProxy
	proto.Message
}

// DispatcherService implements the dispatcher service
type DispatcherService struct {
	dispid                uint16
	config                *config.DispatcherConfig
	games                 []*gameDispatchInfo
	bootGames             []int
	gates                 []*dispatcherClientProxy
	messageQueue          chan dispatcherMessage
	chooseClientIndex     int
	entityDispatchInfos   map[common.EntityID]*entityDispatchInfo
	registeredServices    map[string]entity.EntityIDSet
	entityIDToServices    map[common.EntityID]common.StringSet
	targetGameOfClient    map[common.ClientID]uint16
	entitySyncInfosToGame []*netutil.Packet // cache entity sync infos to gates
	ticker                <-chan time.Time
}

func newDispatcherService(dispid uint16) *DispatcherService {
	cfg := config.GetDispatcher(dispid)
	gameCount := config.GetGamesNum()
	gateCount := config.GetGatesNum()
	entitySyncInfosToGame := make([]*netutil.Packet, gameCount)
	for i := range entitySyncInfosToGame {
		pkt := netutil.NewPacket()
		pkt.AppendUint16(proto.MT_SYNC_POSITION_YAW_FROM_CLIENT)
		entitySyncInfosToGame[i] = pkt
	}
	ds := &DispatcherService{
		dispid:                dispid,
		config:                cfg,
		messageQueue:          make(chan dispatcherMessage, consts.DISPATCHER_SERVICE_PACKET_QUEUE_SIZE),
		games:                 make([]*gameDispatchInfo, gameCount),
		gates:                 make([]*dispatcherClientProxy, gateCount),
		chooseClientIndex:     0,
		entityDispatchInfos:   map[common.EntityID]*entityDispatchInfo{},
		entityIDToServices:    map[common.EntityID]common.StringSet{},
		registeredServices:    map[string]entity.EntityIDSet{},
		targetGameOfClient:    map[common.ClientID]uint16{},
		entitySyncInfosToGame: entitySyncInfosToGame,
		ticker:                time.Tick(consts.DISPATCHER_SERVICE_TICK_INTERVAL),
	}

	for i := range ds.games {
		ds.games[i] = &gameDispatchInfo{gameid: uint16(i + 1)}
	}
	ds.recalcBootGames()

	return ds
}

func (service *DispatcherService) messageLoop() {
	for {
		select {
		case msg := <-service.messageQueue:
			dcp := msg.dcp
			msgtype := msg.MsgType
			pkt := msg.Packet
			if msgtype >= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_START && msgtype <= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_STOP {
				service.handleDoSomethingOnSpecifiedClient(dcp, pkt)
			} else {
				switch msgtype {
				case proto.MT_SYNC_POSITION_YAW_FROM_CLIENT:
					service.handleSyncPositionYawFromClient(dcp, pkt)
				case proto.MT_SYNC_POSITION_YAW_ON_CLIENTS:
					service.handleSyncPositionYawOnClients(dcp, pkt)
				case proto.MT_CALL_ENTITY_METHOD:
					service.handleCallEntityMethod(dcp, pkt)
				case proto.MT_CALL_ENTITY_METHOD_FROM_CLIENT:
					service.handleCallEntityMethodFromClient(dcp, pkt)
				case proto.MT_QUERY_SPACE_GAMEID_FOR_MIGRATE:
					service.handleQuerySpaceGameIDForMigrate(dcp, pkt)
				case proto.MT_MIGRATE_REQUEST:
					service.handleMigrateRequest(dcp, pkt)
				case proto.MT_REAL_MIGRATE:
					service.handleRealMigrate(dcp, pkt)
				case proto.MT_CALL_FILTERED_CLIENTS:
					service.handleCallFilteredClientProxies(dcp, pkt)
				case proto.MT_NOTIFY_CLIENT_CONNECTED:
					service.handleNotifyClientConnected(dcp, pkt)
				case proto.MT_NOTIFY_CLIENT_DISCONNECTED:
					service.handleNotifyClientDisconnected(dcp, pkt)
				case proto.MT_LOAD_ENTITY_ANYWHERE:
					service.handleLoadEntityAnywhere(dcp, pkt)
				case proto.MT_NOTIFY_CREATE_ENTITY:
					eid := pkt.ReadEntityID()
					service.handleNotifyCreateEntity(dcp, pkt, eid)
				case proto.MT_NOTIFY_DESTROY_ENTITY:
					eid := pkt.ReadEntityID()
					service.handleNotifyDestroyEntity(dcp, pkt, eid)
				case proto.MT_CREATE_ENTITY_ANYWHERE:
					service.handleCreateEntityAnywhere(dcp, pkt)
				case proto.MT_CALL_NIL_SPACES:
					service.handleCallNilSpaces(dcp, pkt)
				case proto.MT_CANCEL_MIGRATE:
					service.handleCancelMigrate(dcp, pkt)
				case proto.MT_DECLARE_SERVICE:
					service.handleDeclareService(dcp, pkt)
				case proto.MT_SET_GAME_ID:
					// this is a game server
					service.handleSetGameID(dcp, pkt)
				case proto.MT_SET_GATE_ID:
					// this is a gate
					service.handleSetGateID(dcp, pkt)
				case proto.MT_START_FREEZE_GAME:
					// freeze the game
					service.handleStartFreezeGame(dcp, pkt)
				default:
					gwlog.TraceError("unknown msgtype %d from %s", msgtype, dcp)
				}
			}

			pkt.Release()
			break
		case <-service.ticker:
			post.Tick()
			service.sendEntitySyncInfosToGames()
			break
		}
	}
}

func (service *DispatcherService) terminate() {
	gwlog.Infof("Dispatcher terminated gracefully.")
	os.Exit(0)
}

func (service *DispatcherService) delEntityDispatchInfo(entityID common.EntityID) {
	delete(service.entityDispatchInfos, entityID)
}

func (service *DispatcherService) setEntityDispatcherInfoForWrite(entityID common.EntityID) (info *entityDispatchInfo) {
	info = service.entityDispatchInfos[entityID]

	if info == nil {
		info = &entityDispatchInfo{}
		service.entityDispatchInfos[entityID] = info
	}

	return
}

func (service *DispatcherService) String() string {
	return fmt.Sprintf("DispatcherService<%d>", dispid)
}

func (service *DispatcherService) run() {
	host := fmt.Sprintf("%s:%d", service.config.BindIp, service.config.BindPort)
	binutil.PrintSupervisorTag(consts.DISPATCHER_STARTED_TAG)
	go gwutils.RepeatUntilPanicless(service.messageLoop)
	netutil.ServeTCPForever(host, service)
}

// ServeTCPConnection handles dispatcher client connections to dispatcher
func (service *DispatcherService) ServeTCPConnection(conn net.Conn) {
	tcpConn := conn.(*net.TCPConn)
	tcpConn.SetReadBuffer(consts.DISPATCHER_CLIENT_PROXY_READ_BUFFER_SIZE)
	tcpConn.SetWriteBuffer(consts.DISPATCHER_CLIENT_PROXY_WRITE_BUFFER_SIZE)

	client := newDispatcherClientProxy(service, conn)
	client.serve()
}

func (service *DispatcherService) handleSetGameID(dcp *dispatcherClientProxy, pkt *netutil.Packet) {

	gameid := pkt.ReadUint16()
	isReconnect := pkt.ReadBool()
	isRestore := pkt.ReadBool()
	isBanBootEntity := pkt.ReadBool()

	gwlog.Infof("%s: connection %s set gameid=%d, isReconnect=%v, isRestore=%v, isBanBootEntity=%v", service, dcp, gameid, isReconnect, isRestore, isBanBootEntity)

	if gameid <= 0 {
		gwlog.Panicf("invalid gameid: %d", gameid)
	}
	if dcp.gameid > 0 || dcp.gateid > 0 {
		gwlog.Panicf("already set gameid=%d, gateid=%d", dcp.gameid, dcp.gateid)
	}
	dcp.gameid = gameid
	dcp.SetAutoFlush(consts.DISPATCHER_CLIENT_PROXY_WRITE_FLUSH_INTERVAL) // TODO: why start autoflush after gameid is set ?

	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleSetGameID: dcp=%s, gameid=%d, isReconnect=%v", service, dcp, gameid, isReconnect)
	}

	gdi := service.games[gameid-1]
	if gdi.clientProxy != nil {
		gdi.clientProxy.Close()
		service.handleGameDisconnected(gdi.clientProxy)
	}

	oldIsBanBootEntity := gdi.isBanBootEntity
	gdi.isBanBootEntity = isBanBootEntity
	gdi.setClientProxy(dcp) // should be nil, unless reconnect
	gdi.unblock()           // unlock game dispatch info if new game is connected
	if oldIsBanBootEntity != isBanBootEntity {
		service.recalcBootGames() // recalc if necessary
	}

	// reuse the packet to send SET_GAMEID_ACK with all connected gameids
	connectedGameIDs := service.getConnectedGameIDs()
	dcp.SendSetGameIDAck(connectedGameIDs)
	service.sendNotifyGameConnected(gameid)

	//if !isRestore {
	//	// notify all games that all games connected to dispatcher now!
	//	if service.dispid == 1 && service.isAllGameClientsConnected() {
	//		pkt.ClearPayload() // reuse this packet
	//		pkt.AppendUint16(proto.MT_NOTIFY_ALL_GAMES_CONNECTED)
	//		if olddcp == nil {
	//			// for the first time that all games connected to dispatcher, notify all games
	//			gwlog.Infof("All games(%d) are connected", len(service.games))
	//			service.broadcastToGames(pkt)
	//		} else { // dispatcher reconnected, only notify this game
	//			dcp.SendPacket(pkt)
	//		}
	//	}
	//}
	//
	//if olddcp != nil && !isReconnect && !isRestore {
	//	// game was connected, but a new instance is replaced, so we need to wipe the entities on that game
	//	service.cleanupEntitiesOfGame(gameid)
	//}
	return
}

func (service *DispatcherService) getConnectedGameIDs() (gameids []uint16) {
	for _, gdi := range service.games {
		if gdi.clientProxy != nil {
			gameids = append(gameids, gdi.gameid)
		}
	}
	return
}

//
//func (service *DispatcherService) sendSetGameIDAck(pkt *netutil.Packet) {
//}

func (service *DispatcherService) sendNotifyGameConnected(gameid uint16) {
	pkt := proto.MakeNotifyGameConnectedPacket(gameid)
	service.broadcastToGamesExcept(pkt, gameid)
	pkt.Release()
}

func (service *DispatcherService) handleSetGateID(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	gateid := pkt.ReadUint16()
	if gateid <= 0 {
		gwlog.Panicf("invalid gateid: %d", gateid)
	}
	if dcp.gameid > 0 || dcp.gateid > 0 {
		gwlog.Panicf("already set gameid=%d, gateid=%d", dcp.gameid, dcp.gateid)
	}

	dcp.gateid = gateid
	dcp.SetAutoFlush(consts.DISPATCHER_CLIENT_PROXY_WRITE_FLUSH_INTERVAL) // TODO: why start autoflush after gameid is set ?
	gwlog.Infof("Gate %d is connected: %s", gateid, dcp)

	olddcp := service.gates[gateid-1]
	if olddcp != nil {
		gwlog.Warnf("Gate %d connection %s is replaced by new connection %s", gateid, olddcp, dcp)
		olddcp.Close()
		service.handleGateDisconnected(olddcp)
	}

	service.gates[gateid-1] = dcp
}

func (service *DispatcherService) handleStartFreezeGame(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	// freeze the game, which block all entities of that game
	gwlog.Infof("Handling start freeze game ...")
	gameid := dcp.gameid
	gdi := service.games[gameid-1]

	gdi.block(consts.DISPATCHER_FREEZE_GAME_TIMEOUT)

	// tell the game to start real freeze, re-using the packet
	pkt.ClearPayload()
	pkt.AppendUint16(proto.MT_START_FREEZE_GAME_ACK)
	pkt.AppendUint16(dispid)
	dcp.SendPacket(pkt)
}

func (service *DispatcherService) isAllGameClientsConnected() bool {
	for _, gdi := range service.games {
		if !gdi.isConnected() {
			return false
		}
	}
	return true
}

func (service *DispatcherService) connectedGameClientsNum() int {
	num := 0
	for _, gdi := range service.games {
		if gdi.isConnected() {
			num += 1
		}
	}
	return num
}

func (service *DispatcherService) dispatchPacketToGame(gameid uint16, pkt *netutil.Packet) error {
	return service.games[gameid-1].dispatchPacket(pkt)
}

func (service *DispatcherService) dispatcherClientOfGate(gateid uint16) *dispatcherClientProxy {
	return service.gates[gateid-1]
}

// Choose a dispatcher client for sending Anywhere packets
func (service *DispatcherService) chooseGame() *gameDispatchInfo {
	gdi := service.games[service.chooseClientIndex]
	service.chooseClientIndex = (service.chooseClientIndex + 1) % len(service.games)
	return gdi
}

// Choose a dispatcher client for sending Anywhere packets
func (service *DispatcherService) chooseGameForBootEntity() *gameDispatchInfo {
	idx := service.bootGames[rand.Intn(len(service.bootGames))]
	gdi := service.games[idx]
	return gdi
}

func (service *DispatcherService) handleDispatcherClientDisconnect(dcp *dispatcherClientProxy) {
	// nothing to do when client disconnected
	defer func() {
		if err := recover(); err != nil {
			gwlog.Errorf("handleDispatcherClientDisconnect paniced: %v", err)
		}
	}()
	gwlog.Warnf("%s disconnected", dcp)
	if dcp.gateid > 0 {
		// gate disconnected, notify all clients disconnected
		service.handleGateDisconnected(dcp)
	} else if dcp.gameid > 0 {
		service.handleGameDisconnected(dcp)
	}
}

func (service *DispatcherService) handleGateDisconnected(dcp *dispatcherClientProxy) {
	gateid := dcp.gateid
	gwlog.Warnf("Gate %d connection %s is down!", gateid, dcp)

	curdcp := service.gates[gateid-1]
	if curdcp != dcp {
		gwlog.Errorf("Gate %d connection %s is down, but the current connection is %s", gateid, dcp, curdcp)
		return
	}

	service.gates[gateid-1] = nil

	pkt := netutil.NewPacket()
	pkt.AppendUint16(proto.MT_NOTIFY_GATE_DISCONNECTED)
	pkt.AppendUint16(gateid)
	service.broadcastToGames(pkt)
	pkt.Release()
}

func (service *DispatcherService) handleGameDisconnected(dcp *dispatcherClientProxy) {
	gameid := dcp.gameid
	gwlog.Errorf("%s: game %d is down: %s", service, gameid, dcp)
	gdi := service.games[gameid-1]
	if dcp != gdi.clientProxy {
		// this connection is not the current connection
		gwlog.Errorf("%s: game%d connection %s is disconnected, but the current connection is %s", service, gameid, dcp, gdi.clientProxy)
		return
	}

	gdi.clientProxy = nil // connection down, set clientProxy = nil
	if !gdi.isBlocked {
		// game is down, we need to clear all
		service.cleanupGameInfo(gameid, gdi)
	} else {
		// game is freezed, wait for restore, setup a timer to cleanup later if restore is not success
	}

}

func (service *DispatcherService) cleanupGameInfo(gameid uint16, gdi *gameDispatchInfo) {
	// clear all entities and other infos for the game
	gwlog.Infof("%s: cleanup game info: game%d", service, gameid)
	service.cleanupEntitiesOfGame(gameid)
	gdi.clearPendingPackets()
}

func (service *DispatcherService) cleanupEntitiesOfGame(gameid uint16) {
	cleanEids := entity.EntityIDSet{} // get all clean eids
	for eid, dispatchInfo := range service.entityDispatchInfos {
		if dispatchInfo.gameid == gameid {
			cleanEids.Add(eid)
		}
	}

	//// for all services whose entity is cleaned, notify all games that the service is down
	//undeclaredServices := common.StringSet{}
	//for serviceName, serviceEids := range service.registeredServices {
	//	var serviceRemoveEids []common.EntityID
	//	for serviceEid := range serviceEids {
	//		if cleanEids.Contains(serviceEid) { // this service entity is down, tell other games
	//			undeclaredServices.Add(serviceName)
	//			serviceRemoveEids = append(serviceRemoveEids, serviceEid)
	//			service.handleServiceDown(gameid, serviceName, serviceEid)
	//		}
	//	}
	//
	//	for _, eid := range serviceRemoveEids {
	//		serviceEids.Del(eid)
	//	}
	//}

	for eid := range cleanEids {
		service.cleanupEntityInfo(eid)
	}

	gwlog.Infof("%s: game%d is down, %d entities cleaned", service, gameid, len(cleanEids))
}

// Entity is create on the target game
func (service *DispatcherService) handleNotifyCreateEntity(dcp *dispatcherClientProxy, pkt *netutil.Packet, entityID common.EntityID) {
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleNotifyCreateEntity: dcp=%s, entityID=%s", service, dcp, entityID)
	}
	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(entityID)
	entityDispatchInfo.gameid = dcp.gameid
	entityDispatchInfo.unblock()
}

func (service *DispatcherService) handleNotifyDestroyEntity(dcp *dispatcherClientProxy, pkt *netutil.Packet, entityID common.EntityID) {
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleNotifyDestroyEntity: dcp=%s, entityID=%s", service, dcp, entityID)
	}
	service.cleanupEntityInfo(entityID)
}

func (service *DispatcherService) cleanupEntityInfo(entityID common.EntityID) {
	service.delEntityDispatchInfo(entityID)
	if services, ok := service.entityIDToServices[entityID]; ok {
		for serviceName := range services {
			service.registeredServices[serviceName].Del(entityID)
		}
		delete(service.entityIDToServices, entityID)
		gwlog.Warnf("%s: entity %s is cleaned up, undeclared servies %v", service, entityID, services.ToList())
	}
}

func (service *DispatcherService) handleNotifyClientConnected(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	clientid := pkt.ReadClientID()
	targetGame := service.chooseGameForBootEntity()

	service.targetGameOfClient[clientid] = targetGame.gameid // owner is not determined yet, set to "" as placeholder

	if consts.DEBUG_CLIENTS {
		gwlog.Debugf("Target game of client %s is SET to %v on connected", clientid, targetGame.gameid)
	}

	pkt.AppendUint16(dcp.gateid)
	targetGame.dispatchPacket(pkt)
}

func (service *DispatcherService) handleNotifyClientDisconnected(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	clientid := pkt.ReadClientID() // client disconnected

	targetSid := service.targetGameOfClient[clientid]
	delete(service.targetGameOfClient, clientid)

	if consts.DEBUG_CLIENTS {
		gwlog.Debugf("Target game of client %s is %v, disconnecting ...", clientid, targetSid)
	}

	if targetSid != 0 { // if found the owner, tell it
		service.dispatchPacketToGame(targetSid, pkt)
	}
}

func (service *DispatcherService) handleLoadEntityAnywhere(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	//typeName := pkt.ReadVarStr()
	//eid := pkt.ReadEntityID()
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleLoadEntityAnywhere: dcp=%s, pkt=%v", service, dcp, pkt.Payload())
	}
	eid := pkt.ReadEntityID() // field 1

	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(eid)

	if entityDispatchInfo.gameid == 0 { // entity not loaded, try load now
		dgi := service.chooseGame()
		entityDispatchInfo.gameid = dgi.gameid
		entityDispatchInfo.blockRPC(consts.DISPATCHER_LOAD_TIMEOUT)
		dgi.dispatchPacket(pkt)
	}
}

func (service *DispatcherService) handleCreateEntityAnywhere(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleCreateEntityAnywhere: dcp=%s, pkt=%s", service, dcp, pkt.Payload())
	}

	entityid := pkt.ReadEntityID()

	gdi := service.chooseGame()
	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(entityid)
	entityDispatchInfo.gameid = gdi.gameid // setup gameid of entity
	gdi.dispatchPacket(pkt)
}

func (service *DispatcherService) handleDeclareService(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	entityID := pkt.ReadEntityID()
	serviceName := pkt.ReadVarStr()
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleDeclareService: dcp=%s, entityID=%s, serviceName=%s", service, dcp, entityID, serviceName)
	}

	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(entityID)
	entityDispatchInfo.gameid = dcp.gameid

	eids, ok := service.registeredServices[serviceName]
	if !ok {
		eids = entity.EntityIDSet{}
		service.registeredServices[serviceName] = eids
	}

	if !eids.Contains(entityID) {
		eids.Add(entityID)
		services, ok := service.entityIDToServices[entityID]
		if !ok {
			services = common.StringSet{}
			service.entityIDToServices[entityID] = services
		}
		services.Add(serviceName)
		service.broadcastToGames(pkt)
	}
}

func (service *DispatcherService) handleServiceDown(gameid uint16, serviceName string, eid common.EntityID) {
	gwlog.Warnf("%s: service %s: entity %s is down!", service, serviceName, eid)
	pkt := netutil.NewPacket()
	pkt.AppendUint16(proto.MT_UNDECLARE_SERVICE)
	pkt.AppendEntityID(eid)
	pkt.AppendVarStr(serviceName)
	service.broadcastToGamesExcept(pkt, gameid)
	pkt.Release()
}

func (service *DispatcherService) handleCallEntityMethod(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	entityID := pkt.ReadEntityID()

	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleCallEntityMethod: dcp=%s, entityID=%s", service, dcp, entityID)
	}

	entityDispatchInfo := service.entityDispatchInfos[entityID]
	if entityDispatchInfo != nil {
		entityDispatchInfo.dispatchPacket(pkt)
	} else {
		gwlog.Warnf("%s: entity %s is called by other entity, but dispatch info is not found", service, entityID)
	}
}

func (service *DispatcherService) handleCallNilSpaces(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	// send the packet to all games
	exceptGameID := pkt.ReadUint16()
	service.broadcastToGamesExcept(pkt, exceptGameID)
}

func (service *DispatcherService) handleSyncPositionYawOnClients(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	gateid := pkt.ReadUint16()
	service.dispatcherClientOfGate(gateid).SendPacket(pkt)
}

func (service *DispatcherService) handleSyncPositionYawFromClient(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	// This sync packet contains position-yaw of multiple entities from a gate. Cache the packet to be send before flush?
	payload := pkt.UnreadPayload()

	for i := 0; i < len(payload); i += proto.SYNC_INFO_SIZE_PER_ENTITY + common.ENTITYID_LENGTH {
		eid := common.EntityID(payload[i : i+common.ENTITYID_LENGTH]) // the first bytes of each entry is the EntityID

		entityDispatchInfo := service.entityDispatchInfos[eid]
		if entityDispatchInfo == nil {
			gwlog.Warnf("%s: entity %s is synced from client, but dispatch info is not found", service, eid)
			continue
		}

		gameid := entityDispatchInfo.gameid

		// put this sync info to the pending queue of target game
		// concat to the end of queue
		pkt := service.entitySyncInfosToGame[gameid-1]
		pkt.AppendBytes(payload[i : i+proto.SYNC_INFO_SIZE_PER_ENTITY+common.ENTITYID_LENGTH])
	}
}

func (service *DispatcherService) sendEntitySyncInfosToGames() {
	for gameidx, pkt := range service.entitySyncInfosToGame {
		if pkt.GetPayloadLen() <= 2 {
			continue
		}

		service.games[gameidx].dispatchPacket(pkt)
		pkt.Release()

		// send the entity sync infos to this game
		pkt = netutil.NewPacket()
		pkt.AppendUint16(proto.MT_SYNC_POSITION_YAW_FROM_CLIENT)
		service.entitySyncInfosToGame[gameidx] = pkt
	}
}

func (service *DispatcherService) handleCallEntityMethodFromClient(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	entityID := pkt.ReadEntityID()

	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleCallEntityMethodFromClient: entityID=%s, payload=%v", service, entityID, pkt.Payload())
	}

	entityDispatchInfo := service.entityDispatchInfos[entityID]
	if entityDispatchInfo != nil {
		entityDispatchInfo.dispatchPacket(pkt)
	} else {
		gwlog.Warnf("%s: entity %s is called by client, but dispatch info is not found", service, entityID)
	}
}

func (service *DispatcherService) handleDoSomethingOnSpecifiedClient(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	gid := pkt.ReadUint16()
	service.dispatcherClientOfGate(gid).SendPacket(pkt)
}

func (service *DispatcherService) handleCallFilteredClientProxies(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	service.broadcastToGates(pkt)
}

func (service *DispatcherService) handleQuerySpaceGameIDForMigrate(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	spaceid := pkt.ReadEntityID()
	if consts.DEBUG_PACKETS {
		gwlog.Debugf("%s.handleQuerySpaceGameIDForMigrate: spaceid=%s", service, spaceid)
	}

	spaceDispatchInfo := service.entityDispatchInfos[spaceid]
	var gameid uint16
	if spaceDispatchInfo != nil {
		gameid = spaceDispatchInfo.gameid
	}
	pkt.AppendUint16(gameid)
	// send the packet back
	dcp.SendPacket(pkt)
}

func (service *DispatcherService) handleMigrateRequest(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	entityID := pkt.ReadEntityID()
	spaceID := pkt.ReadEntityID()
	spaceGameID := pkt.ReadUint16()

	if consts.DEBUG_PACKETS {
		gwlog.Debugf("Entity %s is migrating to space %s @ game%d", entityID, spaceID, spaceGameID)
	}

	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(entityID)
	entityDispatchInfo.blockRPC(consts.DISPATCHER_MIGRATE_TIMEOUT)
	dcp.SendPacket(pkt)
}

func (service *DispatcherService) handleCancelMigrate(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	entityid := pkt.ReadEntityID()

	if consts.DEBUG_PACKETS {
		gwlog.Debugf("Entity %s cancelled migrating", entityid)
	}

	entityDispatchInfo := service.entityDispatchInfos[entityid]
	if entityDispatchInfo != nil {
		entityDispatchInfo.unblock()
	}
}

func (service *DispatcherService) handleRealMigrate(dcp *dispatcherClientProxy, pkt *netutil.Packet) {
	// get spaceID and make sure it exists
	eid := pkt.ReadEntityID()
	targetGame := pkt.ReadUint16() // target game of migration
	// target space is not checked for existence, because we relay the packet anyway

	hasClient := pkt.ReadBool()
	var clientid common.ClientID
	if hasClient {
		clientid = pkt.ReadClientID()
	}

	// mark the eid as migrating done
	entityDispatchInfo := service.setEntityDispatcherInfoForWrite(eid)

	entityDispatchInfo.gameid = targetGame
	service.targetGameOfClient[clientid] = targetGame // migrating also change target game of client

	if consts.DEBUG_CLIENTS {
		gwlog.Debugf("Target game of client %s is migrated to %v along with owner %s", clientid, targetGame, eid)
	}

	service.dispatchPacketToGame(targetGame, pkt)
	// send the cached calls to target game
	entityDispatchInfo.unblock()
}

func (service *DispatcherService) broadcastToGames(pkt *netutil.Packet) {
	for idx, gdi := range service.games {
		if gdi != nil {
			gdi.dispatchPacket(pkt)
		} else {
			gwlog.Errorf("Game %d is not connected to dispatcher when broadcasting", idx+1)
		}
	}
}

func (service *DispatcherService) broadcastToGamesExcept(pkt *netutil.Packet, exceptGameID uint16) {
	for idx, gdi := range service.games {
		if uint16(idx+1) == exceptGameID {
			continue
		}
		if gdi != nil {
			gdi.dispatchPacket(pkt)
		} else {
			gwlog.Errorf("Game %d is not connected to dispatcher when broadcasting", idx+1)
		}
	}
}

func (service *DispatcherService) broadcastToGates(pkt *netutil.Packet) {
	for idx, dcp := range service.gates {
		if dcp != nil {
			dcp.SendPacket(pkt)
		} else {
			gwlog.Errorf("Gate %d is not connected to dispatcher when broadcasting", idx+1)
		}
	}
}

func (service *DispatcherService) recalcBootGames() {
	var candidates []int
	for i, gdi := range service.games {
		if !gdi.isBanBootEntity {
			candidates = append(candidates, i)
		}
	}
	service.bootGames = candidates
}

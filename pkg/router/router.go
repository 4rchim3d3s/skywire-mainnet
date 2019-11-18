// Package router implements package router for skywire visor.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/rpc"
	"sync"
	"time"

	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/skycoin/src/util/logging"

	"github.com/SkycoinProject/skywire-mainnet/internal/skyenv"
	"github.com/SkycoinProject/skywire-mainnet/pkg/routefinder/rfclient"
	"github.com/SkycoinProject/skywire-mainnet/pkg/routing"
	"github.com/SkycoinProject/skywire-mainnet/pkg/setup/setupclient"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet"
	"github.com/SkycoinProject/skywire-mainnet/pkg/transport"
)

//go:generate mockery -name Router -case underscore -inpkg

const (
	// DefaultRouteKeepAlive is the default expiration interval for routes
	DefaultRouteKeepAlive = 2 * time.Hour // TODO(nkryuchkov): change
	acceptSize            = 1024

	minHops = 0
	maxHops = 50
)

var log = logging.MustGetLogger("router")

// Config configures Router.
type Config struct {
	Logger           *logging.Logger
	PubKey           cipher.PubKey
	SecKey           cipher.SecKey
	TransportManager *transport.Manager
	RoutingTable     routing.Table
	RouteFinder      rfclient.Client
	SetupNodes       []cipher.PubKey
}

// SetDefaults sets default values for certain empty values.
func (c *Config) SetDefaults() {
	if c.Logger == nil {
		c.Logger = log
	}
}

type DialOptions struct {
	MinForwardRts int
	MaxForwardRts int
	MinConsumeRts int
	MaxConsumeRts int
}

// TODO(nkryuchkov): return pointer, make struct val unexported
func DefaultDialOptions() DialOptions {
	return DialOptions{
		MinForwardRts: 1,
		MaxForwardRts: 1,
		MinConsumeRts: 1,
		MaxConsumeRts: 1,
	}
}

// TODO(nkryuchkov): consider moving to visor package
type Router interface {
	io.Closer

	// DialRoutes dials to a given visor of 'rPK'.
	// 'lPort'/'rPort' specifies the local/remote ports respectively.
	// A nil 'opts' input results in a value of '1' for all DialOptions fields.
	// A single call to DialRoutes should perform the following:
	// - Find routes via RouteFinder (in one call).
	// - Setup routes via SetupNode (in one call).
	// - Save to routing.Table and internal RouteGroup map.
	// - Return RouteGroup if successful.
	DialRoutes(ctx context.Context, rPK cipher.PubKey, lPort, rPort routing.Port, opts *DialOptions) (*RouteGroup, error)

	// AcceptRoutes should block until we receive an visorAddRules packet from SetupNode that contains ConsumeRule(s) or ForwardRule(s).
	// Then the following should happen:
	// - Save to routing.Table and internal RouteGroup map.
	// - Return the RoutingGroup.
	AcceptRoutes(context.Context) (*RouteGroup, error)

	SaveRoutingRules(rules ...routing.Rule) error

	ReserveKeys(n int) ([]routing.RouteID, error)

	IntroduceRules(rules routing.EdgeRules) error

	Serve(context.Context) error

	SetupIsTrusted(cipher.PubKey) bool
}

// Router implements node.PacketRouter. It manages routing table by
// communicating with setup nodes, forward packets according to local
// rules and manages loops for apps.
type router struct {
	mx           sync.Mutex
	once         sync.Once
	done         chan struct{}
	wg           sync.WaitGroup
	conf         *Config
	logger       *logging.Logger
	n            *snet.Network
	sl           *snet.Listener
	trustedNodes map[cipher.PubKey]struct{}
	tm           *transport.Manager
	rt           routing.Table
	rfc          rfclient.Client                         // route finder client
	rgs          map[routing.RouteDescriptor]*RouteGroup // route groups to push incoming reads from transports.
	rpcSrv       *rpc.Server
	accept       chan routing.EdgeRules
}

// New constructs a new Router.
func New(n *snet.Network, config *Config) (Router, error) {
	config.SetDefaults()

	sl, err := n.Listen(snet.DmsgType, skyenv.DmsgAwaitSetupPort)
	if err != nil {
		return nil, err
	}

	trustedNodes := make(map[cipher.PubKey]struct{})
	for _, node := range config.SetupNodes {
		trustedNodes[node] = struct{}{}
	}

	r := &router{
		conf:         config,
		logger:       config.Logger,
		n:            n,
		tm:           config.TransportManager,
		rt:           config.RoutingTable,
		sl:           sl,
		rfc:          config.RouteFinder,
		rpcSrv:       rpc.NewServer(),
		accept:       make(chan routing.EdgeRules, acceptSize),
		trustedNodes: trustedNodes,
	}

	if err := r.rpcSrv.Register(NewRPCGateway(r)); err != nil {
		return nil, fmt.Errorf("failed to register RPC server")
	}

	return r, nil
}

// DialRoutes dials to a given visor of 'rPK'.
// 'lPort'/'rPort' specifies the local/remote ports respectively.
// A nil 'opts' input results in a value of '1' for all DialOptions fields.
// A single call to DialRoutes should perform the following:
// - Find routes via RouteFinder (in one call).
// - Setup routes via SetupNode (in one call).
// - Save to routing.Table and internal RouteGroup map.
// - Return RouteGroup if successful.
func (r *router) DialRoutes(ctx context.Context, rPK cipher.PubKey, lPort, rPort routing.Port, opts *DialOptions) (*RouteGroup, error) {
	lPK := r.conf.PubKey
	forwardDesc := routing.NewRouteDescriptor(lPK, rPK, lPort, rPort)

	forwardPath, reversePath, err := r.fetchBestRoutes(lPK, rPK, opts)
	if err != nil {
		return nil, fmt.Errorf("route finder: %s", err)
	}

	req := routing.BidirectionalRoute{
		Desc:      forwardDesc,
		KeepAlive: DefaultRouteKeepAlive,
		Forward:   forwardPath,
		Reverse:   reversePath,
	}

	rules, err := setupclient.DialRouteGroup(ctx, r.logger, r.n, r.conf.SetupNodes, req)
	if err != nil {
		return nil, err
	}

	if err := r.SaveRoutingRules(rules.Forward, rules.Reverse); err != nil {
		return nil, err
	}

	rg := r.saveRouteGroupRules(rules)

	r.logger.Infof("Created new routes to %s on port %d", rPK, lPort)
	return rg, nil
}

// AcceptsRoutes should block until we receive an AddRules packet from SetupNode that contains ConsumeRule(s) or ForwardRule(s).
// Then the following should happen:
// - Save to routing.Table and internal RouteGroup map.
// - Return the RoutingGroup.
func (r *router) AcceptRoutes(ctx context.Context) (*RouteGroup, error) {
	var rules routing.EdgeRules
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case rules = <-r.accept:
		break
	}

	if err := r.SaveRoutingRules(rules.Forward, rules.Reverse); err != nil {
		return nil, err
	}

	rg := r.saveRouteGroupRules(rules)
	return rg, nil
}

// Serve starts transport listening loop.
func (r *router) Serve(ctx context.Context) error {
	r.logger.Info("Starting router")

	go r.serveTransportManager(ctx)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.serveSetup()
	}()

	r.tm.Serve(ctx)
	return nil
}

func (r *router) serveTransportManager(ctx context.Context) {
	for {
		packet, err := r.tm.ReadPacket()
		if err != nil {
			r.logger.WithError(err).Errorf("Failed to read packet")
			return
		}

		if err := r.handleTransportPacket(ctx, packet); err != nil {
			if err == transport.ErrNotServing {
				r.logger.WithError(err).Warnf("Stopped serving Transport.")
				return
			}
			r.logger.Warnf("Failed to handle transport frame: %v", err)
		}
	}
}

func (r *router) serveSetup() {
	for {
		conn, err := r.sl.AcceptConn()
		if err != nil {
			r.logger.WithError(err).Warnf("setup client stopped serving")
		}

		if !r.SetupIsTrusted(conn.RemotePK()) {
			r.logger.Warnf("closing conn from untrusted setup node: %v", conn.Close())
			continue
		}
		r.logger.Infof("handling setup request: setupPK(%s)", conn.RemotePK())

		go r.rpcSrv.ServeConn(conn)

		if err := conn.Close(); err != nil {
			log.WithError(err).Warn("Failed to close connection")
		}
	}
}

func (r *router) saveRouteGroupRules(rules routing.EdgeRules) *RouteGroup {
	r.mx.Lock()
	defer r.mx.Unlock()

	rg, ok := r.rgs[rules.Desc]
	if !ok || rg == nil {
		rg = NewRouteGroup(r.rt, rules.Desc)
		r.rgs[rules.Desc] = rg
	}

	rg.fwd = append(rg.fwd, rules.Forward)
	rg.rvs = append(rg.fwd, rules.Reverse)

	tp := r.tm.Transport(rules.Forward.NextTransportID())
	rg.tps = append(rg.tps, tp)

	return rg
}

func (r *router) handleTransportPacket(ctx context.Context, packet routing.Packet) error {
	switch packet.Type() {
	case routing.DataPacket:
		return r.handleDataPacket(ctx, packet)
	case routing.ClosePacket:
		return r.handleClosePacket(ctx, packet)
	case routing.KeepAlivePacket:
		return r.handleKeepAlivePacket(ctx, packet)
	default:
		return errors.New("unknown packet type")
	}
}

func (r *router) handleDataPacket(ctx context.Context, packet routing.Packet) error {
	rule, err := r.GetRule(packet.RouteID())
	if err != nil {
		return err
	}

	desc := rule.RouteDescriptor()
	rg, ok := r.routeGroup(desc)
	if !ok {
		return errors.New("route descriptor does not exist")
	}
	if rg == nil {
		return errors.New("RouteGroup is nil")
	}

	r.logger.Infof("Got new remote packet with route ID %d. Using rule: %s", packet.RouteID(), rule)

	if t := rule.Type(); t == routing.RuleForward || t == routing.RuleIntermediaryForward {
		return r.forwardPacket(ctx, packet.Payload(), rule)
	}

	if rg.isClosing() {
		return io.ErrClosedPipe
	}

	rg.mu.Lock()
	defer rg.mu.Unlock()

	select {
	case <-rg.done:
		return io.ErrClosedPipe
	case rg.readCh <- packet.Payload():
		return nil
	}
}

func (r *router) handleClosePacket(ctx context.Context, packet routing.Packet) error {
	// TODO(nkryuchkov): implement
	return nil
}

func (r *router) handleKeepAlivePacket(ctx context.Context, packet routing.Packet) error {
	// TODO(nkryuchkov): implement
	return nil
}

// GetRule gets routing rule.
func (r *router) GetRule(routeID routing.RouteID) (routing.Rule, error) {
	rule, err := r.rt.Rule(routeID)
	if err != nil {
		return nil, fmt.Errorf("routing table: %s", err)
	}

	if rule == nil {
		return nil, errors.New("unknown RouteID")
	}

	// TODO(evanlinjin): This is a workaround for ensuring the read-in rule is of the correct size.
	// Sometimes it is not, causing a segfault later down the line.
	if len(rule) < routing.RuleHeaderSize {
		return nil, errors.New("corrupted rule")
	}

	return rule, nil
}

// Close safely stops Router.
func (r *router) Close() error {
	if r == nil {
		return nil
	}

	r.logger.Info("Closing all App connections and Loops")

	r.once.Do(func() {
		close(r.done)

		r.mx.Lock()
		close(r.accept)
		r.mx.Unlock()
	})

	if err := r.sl.Close(); err != nil {
		r.logger.WithError(err).Warnf("closing route_manager returned error")
	}
	r.wg.Wait()

	return r.tm.Close()
}

func (r *router) forwardPacket(ctx context.Context, payload []byte, rule routing.Rule) error {
	tp := r.tm.Transport(rule.NextTransportID())
	if tp == nil {
		return errors.New("unknown transport")
	}
	packet := routing.MakeDataPacket(rule.KeyRouteID(), payload)
	if err := tp.WritePacket(ctx, packet); err != nil {
		return err
	}
	r.logger.Infof("Forwarded packet via Transport %s using rule %d", rule.NextTransportID(), rule.KeyRouteID())
	return nil
}

// RemoveRouteDescriptor removes loop rule.
func (r *router) RemoveRouteDescriptor(desc routing.RouteDescriptor) {
	rules := r.rt.AllRules()
	for _, rule := range rules {
		if rule.Type() != routing.RuleConsume {
			continue
		}

		rd := rule.RouteDescriptor()
		if rd.DstPK() == desc.DstPK() && rd.DstPort() == desc.DstPort() && rd.SrcPort() == desc.SrcPort() {
			r.rt.DelRules([]routing.RouteID{rule.KeyRouteID()})
			return
		}
	}
}

func (r *router) fetchBestRoutes(source, destination cipher.PubKey, opts *DialOptions) (fwd routing.Path, rev routing.Path, err error) {
	// TODO(nkryuchkov): use opts
	if opts == nil {
		defaultOpts := DefaultDialOptions()
		opts = &defaultOpts
	}

	r.logger.Infof("Requesting new routes from %s to %s", source, destination)

	timer := time.NewTimer(time.Second * 10)
	defer timer.Stop()

	forward := [2]cipher.PubKey{source, destination}
	backward := [2]cipher.PubKey{destination, source}

fetchRoutesAgain:
	ctx := context.Background()
	paths, err := r.conf.RouteFinder.FindRoutes(ctx, []routing.PathEdges{forward, backward},
		&rfclient.RouteOptions{MinHops: minHops, MaxHops: maxHops})
	if err != nil {
		select {
		case <-timer.C:
			return nil, nil, err
		default:
			goto fetchRoutesAgain
		}
	}

	r.logger.Infof("Found routes Forward: %s. Reverse %s", paths[forward], paths[backward])
	return paths[forward][0], paths[backward][0], nil
}

// SetupIsTrusted checks if setup node is trusted.
func (r *router) SetupIsTrusted(sPK cipher.PubKey) bool {
	_, ok := r.trustedNodes[sPK]
	return ok
}

// Saves `rules` to the routing table.
func (r *router) SaveRoutingRules(rules ...routing.Rule) error {
	for _, rule := range rules {
		if err := r.rt.SaveRule(rule); err != nil {
			return fmt.Errorf("routing table: %s", err)
		}

		r.logger.Infof("Save new Routing Rule with ID %d %s", rule.KeyRouteID(), rule)
	}

	return nil
}

func (r *router) ReserveKeys(n int) ([]routing.RouteID, error) {
	return r.rt.ReserveKeys(n)
}

func (r *router) routeGroup(desc routing.RouteDescriptor) (*RouteGroup, bool) {
	r.mx.Lock()
	defer r.mx.Unlock()

	rg, ok := r.rgs[desc]
	return rg, ok
}

func (r *router) IntroduceRules(rules routing.EdgeRules) error {
	select {
	case <-r.done:
		return io.ErrClosedPipe
	default:
		r.mx.Lock()
		defer r.mx.Unlock()

		select {
		case r.accept <- rules:
			return nil
		case <-r.done:
			return io.ErrClosedPipe
		}
	}
}

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/SkycoinProject/dmsg"
	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/skycoin/src/util/logging"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/skycoin/skywire/pkg/setup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkycoinProject/skywire-mainnet/pkg/app"
	"github.com/SkycoinProject/skywire-mainnet/pkg/routefinder/rfclient"
	"github.com/SkycoinProject/skywire-mainnet/pkg/routing"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet"
	"github.com/SkycoinProject/skywire-mainnet/pkg/snet/snettest"
	"github.com/SkycoinProject/skywire-mainnet/pkg/transport"
)

func TestMain(m *testing.M) {
	loggingLevel, ok := os.LookupEnv("TEST_LOGGING_LEVEL")
	if ok {
		lvl, err := logging.LevelFromString(loggingLevel)
		if err != nil {
			log.Fatal(err)
		}
		logging.SetLevel(lvl)
	} else {
		logging.SetLevel(logrus.TraceLevel)
	}
	os.Exit(m.Run())
}

// Ensure that received packets are handled properly in `(*Router).Serve()`.
func TestRouter_Serve(t *testing.T) {
	// We are generating two key pairs - one for the a `Router`, the other to send packets to `Router`.
	keys := snettest.GenKeyPairs(2)

	// create test env
	nEnv := snettest.NewEnv(t, keys)
	defer nEnv.Teardown()
	rEnv := NewTestEnv(t, nEnv.Nets)
	defer rEnv.Teardown()

	// Create routers
	r0Ifc, err := New(nEnv.Nets[0], rEnv.GenRouterConfig(0))
	require.NoError(t, err)
	r0, ok := r0Ifc.(*router)
	require.True(t, ok)
	// go r0.Serve(context.TODO())
	r1Ifc, err := New(nEnv.Nets[1], rEnv.GenRouterConfig(1))
	require.NoError(t, err)
	r1, ok := r1Ifc.(*router)
	require.True(t, ok)
	// go r1.Serve(context.TODO())

	// Create dmsg transport between two `snet.Network` entities.
	tp1, err := rEnv.TpMngrs[1].SaveTransport(context.TODO(), keys[0].PK, dmsg.Type)
	require.NoError(t, err)

	// CLOSURE: clear all rules in all router.
	clearRules := func(routers ...*router) {
		for _, r := range routers {
			rules := r.rt.AllRules()
			for _, rule := range rules {
				r.rt.DelRules([]routing.RouteID{rule.KeyRouteID()})
			}
		}
	}

	// TEST: Ensure handleTransportPacket does as expected.
	// After setting a rule in r0, r0 should forward a packet to r1 (as specified in the given rule)
	// when r0.handleTransportPacket() is called.
	t.Run("handlePacket_fwdRule", func(t *testing.T) {
		defer clearRules(r0, r1)

		// Add a FWD rule for r0.
		fwdRtID, err := r0.ReserveKeys(1)
		require.NoError(t, err)

		fwdRule := routing.ForwardRule(1*time.Hour, fwdRtID[0], routing.RouteID(5), tp1.Entry.ID, keys[1].PK, 0, 0)
		err = r0.rt.SaveRule(fwdRule)
		require.NoError(t, err)

		// Call handleTransportPacket for r0 (this should in turn, use the rule we added).
		packet := routing.MakeDataPacket(fwdRtID[0], []byte("This is a test!"))
		require.NoError(t, r0.handleTransportPacket(context.TODO(), packet))

		// r1 should receive the packet handled by r0.
		recvPacket, err := r1.tm.ReadPacket()
		assert.NoError(t, err)
		assert.Equal(t, packet.Size(), recvPacket.Size())
		assert.Equal(t, packet.Payload(), recvPacket.Payload())
		assert.Equal(t, fwdRtID, packet.RouteID())
	})

	t.Run("handlePacket_intFwdRule", func(t *testing.T) {
		defer clearRules(r0, r1)

		// Add a FWD rule for r0.
		fwdRtID, err := r0.ReserveKeys(1)
		require.NoError(t, err)

		fwdRule := routing.IntermediaryForwardRule(1*time.Hour, fwdRtID[0], routing.RouteID(5), tp1.Entry.ID)
		err = r0.rt.SaveRule(fwdRule)
		require.NoError(t, err)

		// Call handleTransportPacket for r0 (this should in turn, use the rule we added).
		packet := routing.MakeDataPacket(fwdRtID[0], []byte("This is a test!"))
		require.NoError(t, r0.handleTransportPacket(context.TODO(), packet))

		// r1 should receive the packet handled by r0.
		recvPacket, err := r1.tm.ReadPacket()
		assert.NoError(t, err)
		assert.Equal(t, packet.Size(), recvPacket.Size())
		assert.Equal(t, packet.Payload(), recvPacket.Payload())
		assert.Equal(t, fwdRtID, packet.RouteID())
	})

	// TODO(evanlinjin): I'm having so much trouble with this I officially give up.
	t.Run("handlePacket_appRule", func(t *testing.T) {
		const duration = 10 * time.Second
		// time.AfterFunc(duration, func() {
		// 	panic("timeout")
		// })

		defer clearRules(r0, r1)

		// prepare mock-app
		localPort := routing.Port(9)
		cConn, sConn := net.Pipe()

		// mock-app config
		appConf := &app.Config{
			AppName:         "test_app",
			AppVersion:      "1.0",
			ProtocolVersion: supportedProtocolVersion,
		}

		// serve mock-app
		// sErrCh := make(chan error, 1)
		go func() {
			// sErrCh <- r0.ServeApp(sConn, localPort, appConf)
			_ = r0.ServeApp(sConn, localPort, appConf)
			// close(sErrCh)
		}()
		// defer func() {
		// 	assert.NoError(t, cConn.Close())
		// 	assert.NoError(t, <-sErrCh)
		// }()

		a, err := app.New(cConn, appConf)
		require.NoError(t, err)
		// cErrCh := make(chan error, 1)
		go func() {
			conn, err := a.Accept()
			if err == nil {
				fmt.Println("ACCEPTED:", conn.RemoteAddr())
			}
			fmt.Println("FAILED TO ACCEPT")
			// cErrCh <- err
			// close(cErrCh)
		}()
		a.Dial(a.Addr().(routing.Addr))
		// defer func() {
		// 	assert.NoError(t, <-cErrCh)
		// }()

		// Add a APP rule for r0.
		// port8 := routing.Port(8)
		appRule := routing.AppRule(10*time.Minute, 0, routing.RouteID(7), keys[1].PK, localPort, localPort)
		appRtID, err := r0.rm.rt.AddRule(appRule)
		require.NoError(t, err)

		// Call handleTransportPacket for r0.

		// payload is prepended with two bytes to satisfy app.Proto.
		// payload[0] = frame type, payload[1] = id
		rAddr := routing.Addr{PubKey: keys[1].PK, Port: localPort}
		rawRAddr, _ := json.Marshal(rAddr)
		// payload := append([]byte{byte(app.FrameClose), 0}, rawRAddr...)
		packet := routing.MakeDataPacket(appRtID, rawRAddr)
		require.NoError(t, r0.handleTransportPacket(context.TODO(), packet))
	})
}

// TODO (Darkren): fix tests
func TestRouter_Rules(t *testing.T) {
	pk, sk := cipher.GenerateKeyPair()

	env := snettest.NewEnv(t, []snettest.KeyPair{{PK: pk, SK: sk}})
	defer env.Teardown()

	rt := routing.NewTable(routing.Config{GCInterval: 100 * time.Millisecond})

	// We are generating two key pairs - one for the a `Router`, the other to send packets to `Router`.
	keys := snettest.GenKeyPairs(2)

	// create test env
	nEnv := snettest.NewEnv(t, keys)
	defer nEnv.Teardown()
	rEnv := NewTestEnv(t, nEnv.Nets)
	defer rEnv.Teardown()

	rIfc, err := New(nEnv.Nets[0], rEnv.GenRouterConfig(0))
	require.NoError(t, err)
	r, ok := rIfc.(*router)
	require.True(t, ok)

	r.rt = rt

	// CLOSURE: Delete all routing rules.
	clearRules := func() {
		rules := rt.AllRules()
		for _, rule := range rules {
			rt.DelRules([]routing.RouteID{rule.KeyRouteID()})
		}
	}

	// TEST: Set and get expired and unexpired rule.
	t.Run("GetRule", func(t *testing.T) {
		clearRules()

		expiredID, err := r.rt.ReserveKeys(1)
		require.NoError(t, err)

		expiredRule := routing.IntermediaryForwardRule(-10*time.Minute, expiredID[0], 3, uuid.New())
		err = r.rt.SaveRule(expiredRule)
		require.NoError(t, err)

		id, err := r.rt.ReserveKeys(1)
		require.NoError(t, err)

		rule := routing.IntermediaryForwardRule(10*time.Minute, id[0], 3, uuid.New())
		err = r.rt.SaveRule(rule)
		require.NoError(t, err)

		defer r.rt.DelRules([]routing.RouteID{id[0], expiredID[0]})

		// rule should already be expired at this point due to the execution time.
		// However, we'll just a bit to be sure
		time.Sleep(1 * time.Millisecond)

		_, err = r.GetRule(expiredID[0])
		require.Error(t, err)

		_, err = r.GetRule(123)
		require.Error(t, err)

		r, err := r.GetRule(id[0])
		require.NoError(t, err)
		assert.Equal(t, rule, r)
	})

	// TEST: Ensure removing loop rules work properly.
	t.Run("RemoveRouteDescriptor", func(t *testing.T) {
		clearRules()

		pk, _ := cipher.GenerateKeyPair()

		id, err := r.rt.ReserveKeys(1)
		require.NoError(t, err)

		rule := routing.ConsumeRule(10*time.Minute, id[0], pk, 2, 3)
		err = r.rt.SaveRule(rule)
		require.NoError(t, err)

		desc := routing.NewRouteDescriptor(cipher.PubKey{}, pk, 3, 2)
		r.RemoveRouteDescriptor(desc)
		assert.Equal(t, 1, rt.Count())

		desc = routing.NewRouteDescriptor(cipher.PubKey{}, pk, 2, 3)
		r.RemoveRouteDescriptor(desc)
		assert.Equal(t, 0, rt.Count())
	})

	// TEST: Ensure AddRule and DeleteRule requests from a SetupNode does as expected.
	t.Run("AddRemoveRule", func(t *testing.T) {
		clearRules()

		// Add/Remove rules multiple times.
		for i := 0; i < 5; i++ {
			// As setup connections close after a single request completes
			// So we need two pairs of connections.
			requestIDIn, requestIDOut := net.Pipe()
			addIn, addOut := net.Pipe()
			delIn, delOut := net.Pipe()
			errCh := make(chan error, 2)
			go func() {
				errCh <- r.handleSetupConn(requestIDOut) // Receive RequestRegistrationID request.
				errCh <- r.handleSetupConn(addOut)       // Receive AddRule request.
				errCh <- r.handleSetupConn(delOut)       // Receive DeleteRule request.
				close(errCh)
			}()

			// Emulate SetupNode sending RequestRegistrationID request.
			proto := setup.NewSetupProtocol(requestIDIn)
			ids, err := proto.ReserveRtIDs(context.TODO(), 1)
			require.NoError(t, err)

			// Emulate SetupNode sending AddRule request.
			rule := routing.IntermediaryForwardRule(10*time.Minute, ids[0], 3, uuid.New())
			proto = setup.NewSetupProtocol(addIn)
			err = proto.AddRules(context.TODO(), []routing.Rule{rule})
			require.NoError(t, err)

			// Check routing table state after AddRule.
			assert.Equal(t, 1, rt.Count())
			r, err := rt.Rule(ids[0])
			require.NoError(t, err)
			assert.Equal(t, rule, r)

			// Emulate SetupNode sending RemoveRule request.
			require.NoError(t, setup.DeleteRule(context.TODO(), setup.NewSetupProtocol(delIn), ids[0]))

			// Check routing table state after DeleteRule.
			assert.Equal(t, 0, rt.Count())
			r, err = rt.Rule(ids[0])
			assert.Error(t, err)
			assert.Nil(t, r)

			require.NoError(t, requestIDIn.Close())
			require.NoError(t, addIn.Close())
			require.NoError(t, delIn.Close())
			for err := range errCh {
				require.NoError(t, err)
			}
		}
	})

	// TEST: Ensure RoutesCreated request from SetupNode is handled properly.
	t.Run("RoutesCreated", func(t *testing.T) {
		clearRules()

		var inLoop routing.Loop
		var inRule routing.Rule

		r.OnRoutesCreated = func(loop routing.Loop, rule routing.Rule) (err error) {
			inLoop = loop
			inRule = rule
			return nil
		}
		defer func() { r.OnRoutesCreated = nil }()

		in, out := net.Pipe()
		errCh := make(chan error, 1)
		go func() {
			errCh <- r.handleSetupConn(out)
			close(errCh)
		}()
		defer func() {
			require.NoError(t, in.Close())
			require.NoError(t, <-errCh)
		}()

		proto := setup.NewSetupProtocol(in)
		pk, _ := cipher.GenerateKeyPair()

		rule := routing.ConsumeRule(10*time.Minute, 2, pk, 2, 3)
		require.NoError(t, rt.SaveRule(rule))

		rule = routing.IntermediaryForwardRule(10*time.Minute, 1, 3, uuid.New())
		require.NoError(t, rt.SaveRule(rule))

		ld := routing.LoopData{
			Loop: routing.Loop{
				Remote: routing.Addr{
					PubKey: pk,
					Port:   3,
				},
				Local: routing.Addr{
					Port: 2,
				},
			},
			RouteID: 1,
		}
		err := proto.RoutesCreated(context.TODO(), ld)
		require.NoError(t, err)
		assert.Equal(t, rule, inRule)
		assert.Equal(t, routing.Port(2), inLoop.Local.Port)
		assert.Equal(t, routing.Port(3), inLoop.Remote.Port)
		assert.Equal(t, pk, inLoop.Remote.PubKey)
	})
}

type TestEnv struct {
	TpD transport.DiscoveryClient

	TpMngrConfs []*transport.ManagerConfig
	TpMngrs     []*transport.Manager

	teardown func()
}

func NewTestEnv(t *testing.T, nets []*snet.Network) *TestEnv {
	tpD := transport.NewDiscoveryMock()

	mConfs := make([]*transport.ManagerConfig, len(nets))
	ms := make([]*transport.Manager, len(nets))

	for i, n := range nets {
		var err error

		mConfs[i] = &transport.ManagerConfig{
			PubKey:          n.LocalPK(),
			SecKey:          n.LocalSK(),
			DiscoveryClient: tpD,
			LogStore:        transport.InMemoryTransportLogStore(),
		}

		ms[i], err = transport.NewManager(n, mConfs[i])
		require.NoError(t, err)

		go ms[i].Serve(context.TODO())
	}

	teardown := func() {
		for _, m := range ms {
			assert.NoError(t, m.Close())
		}
	}

	return &TestEnv{
		TpD:         tpD,
		TpMngrConfs: mConfs,
		TpMngrs:     ms,
		teardown:    teardown,
	}
}

func (e *TestEnv) GenRouterConfig(i int) *Config {
	return &Config{
		Logger:           logging.MustGetLogger(fmt.Sprintf("router_%d", i)),
		PubKey:           e.TpMngrConfs[i].PubKey,
		SecKey:           e.TpMngrConfs[i].SecKey,
		TransportManager: e.TpMngrs[i],
		RoutingTable:     routing.NewTable(routing.DefaultConfig()),
		RouteFinder:      rfclient.NewMock(),
		SetupNodes:       nil, // TODO
	}
}

func (e *TestEnv) Teardown() {
	e.teardown()
}

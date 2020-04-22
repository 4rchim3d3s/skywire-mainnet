package appdisc

import (
	"strings"
	"time"

	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/skycoin/src/util/logging"
	"github.com/sirupsen/logrus"

	"github.com/SkycoinProject/skywire-mainnet/pkg/app/appcommon"
	"github.com/SkycoinProject/skywire-mainnet/pkg/proxydisc"
	"github.com/SkycoinProject/skywire-mainnet/pkg/skyenv"
)

// Factory creates appdisc.Updater instances based on the app name.
type Factory struct {
	Log            logrus.FieldLogger
	PK             cipher.PubKey
	SK             cipher.SecKey
	UpdateInterval time.Duration
	ProxyDisc      string // Address of proxy-discovery
}

func (f *Factory) setDefaults() {
	if f.Log == nil {
		f.Log = logging.MustGetLogger("appdisc")
	}
	if f.UpdateInterval == 0 {
		f.UpdateInterval = skyenv.AppDiscUpdateInterval
	}
	if f.ProxyDisc == "" {
		f.ProxyDisc = skyenv.DefaultProxyDiscAddr
	}
}

// Updater obtains an updater based on the app name and configuration.
func (f *Factory) Updater(conf appcommon.Config, args []string) (Updater, bool) {

	// Always return empty updater if keys are not set.
	if f.setDefaults(); f.PK.Null() || f.SK.Null() {
		return &emptyUpdater{}, false
	}

	log := f.Log.WithField("appName", conf.Name)

	switch conf.Name {
	case "skysocks":

		// Do not update in proxy discovery if passcode-protected.
		if containsFlag(args, "passcode") {
			return &emptyUpdater{}, false
		}

		return &proxyUpdater{
			client: proxydisc.NewClient(log, proxydisc.Config{
				PK:       f.PK,
				SK:       f.SK,
				Port:     uint16(conf.RoutingPort),
				DiscAddr: f.ProxyDisc,
			}),
			interval: f.UpdateInterval,
		}, true

	default:
		return &emptyUpdater{}, false
	}
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, flag) {
			return true
		}
	}
	return false
}

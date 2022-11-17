package schedulecheck

import (
	"time"

	"github.com/go-co-op/gocron"
	"github.com/go-logr/logr"
	"github.com/pion/ion-sfu/cmd/signal/json-rpc/server"
	cacheredis "github.com/pion/ion-sfu/pkg/cache"
	"github.com/pion/ion-sfu/pkg/sfu"
)

func ScheduleCheckSession(s *sfu.SFU, logger logr.Logger) {
	cron := gocron.NewScheduler(time.UTC)
	cron.Every("5m").Do(func() {
		logger.Info("Schedule check session...")
		ssids, errGet := cacheredis.GetCacheRedis("sessionid")
		if errGet != nil {
			logger.Error(errGet, "Err get redis")
		}
		var deleteSs []sfu.Session

		for id, ss := range s.GetMapSession() {
			check := false
			for _, sid := range ssids {
				if id == sid {
					check = true
					break
				}
			}
			if check == false {
				deleteSs = append(deleteSs, ss)
				if server.PullPeers[id] != nil {
					delete(server.PullPeers, id)
				}
			}
		}

		for _, ss := range deleteSs {
			logger.Info("Schedule remove session...", ss.ID())
			ss.RemoveAllPeer()
		}
	})
	cron.StartAsync()
}

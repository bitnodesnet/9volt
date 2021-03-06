package manager

import (
	"context"
	"fmt"
	"path"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"

	"github.com/9corp/9volt/alerter"
	"github.com/9corp/9volt/base"
	"github.com/9corp/9volt/config"
	"github.com/9corp/9volt/monitor"
	"github.com/9corp/9volt/overwatch"
	"github.com/9corp/9volt/state"
)

type Manager struct {
	MemberID      string
	Log           log.FieldLogger
	Config        *config.Config
	Monitor       *monitor.Monitor
	OverwatchChan chan<- *overwatch.Message

	base.Component
}

func New(cfg *config.Config, messageChannel chan *alerter.Message, stateChannel chan *state.Message, overwatchChan chan<- *overwatch.Message) (*Manager, error) {
	return &Manager{
		MemberID:      cfg.MemberID,
		Config:        cfg,
		Log:           log.WithField("pkg", "manager"),
		Monitor:       monitor.New(cfg, messageChannel, stateChannel),
		OverwatchChan: overwatchChan,
		Component: base.Component{
			Identifier: "manager",
		},
	}, nil
}

func (m *Manager) Start() error {
	m.Log.Info("Starting manager components...")

	m.Component.Ctx, m.Component.Cancel = context.WithCancel(context.Background())

	go m.run()

	return nil
}

func (m *Manager) Stop() error {
	if m.Component.Cancel == nil {
		m.Log.Warning("Looks like .Cancel is nil; is this expected?")
	} else {
		m.Component.Cancel()
	}

	m.Monitor.StopAll()

	return nil
}

func (m *Manager) run() error {
	memberConfigDir := fmt.Sprintf("cluster/members/%v/config", m.MemberID)

	watcher := m.Config.DalClient.NewWatcher(memberConfigDir, true)

	for {
		resp, err := watcher.Next(m.Component.Ctx)
		if err != nil {
			if err.Error() == "context canceled" {
				m.Log.Debug("Received a notice to shutdown")
				break
			}

			m.Config.EQClient.AddWithErrorLog("Unexpected watcher error",
				m.Log, log.Fields{"err": err})

			// Tell overwatch that something bad just happened
			m.OverwatchChan <- &overwatch.Message{
				Error:     fmt.Errorf("Unexpected watcher error: %v", err),
				Source:    fmt.Sprintf("%v.run", m.Identifier),
				ErrorType: overwatch.ETCD_WATCHER_ERROR,
			}

			// Let overwatch determine whether to shut us down
			continue
		}

		if m.ignorableWatcherEvent(resp) {
			m.Log.WithFields(log.Fields{
				"action": resp.Action,
				"key":    resp.Node.Key,
			}).Debug("Received an ignorable watcher event")
			continue
		}

		m.Log.WithFields(log.Fields{
			"action": resp.Action,
			"key":    resp.Node.Key,
			"value":  resp.Node.Value,
		}).Debug("Received watcher event")

		switch resp.Action {
		case "set":
			go m.Monitor.Handle(monitor.START, path.Base(resp.Node.Key), resp.Node.Value)
		case "delete":
			go m.Monitor.Handle(monitor.STOP, path.Base(resp.Node.Key), resp.Node.Value)
		default:
			m.Config.EQClient.AddWithErrorLog("Received an unrecognized action -> skipping",
				m.Log, log.Fields{"action": resp.Action})
		}
	}

	m.Log.Debug("Exiting...")

	return nil
}

// Determine if a specific event can be ignored
func (m *Manager) ignorableWatcherEvent(resp *client.Response) bool {
	if resp == nil {
		m.Log.Debug("Received a nil etcd response - bug?")
		return true
	}

	// Ignore anything that is `config` related
	if path.Base(resp.Node.Key) == "config" {
		return true
	}

	return false
}

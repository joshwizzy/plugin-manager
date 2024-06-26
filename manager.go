package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
)

type ManagerConfig struct {
	HandshakeConfig goplugin.HandshakeConfig
	Plugin          goplugin.Plugin
	RestartConfig   RestartConfig
	Logger          hclog.Logger
}

type RestartConfig struct {
	Managed      bool
	PingInterval time.Duration
	MaxRestarts  int
}

type Manager[C any] struct {
	mu      sync.RWMutex
	Name    string
	killed  chan PluginInfo
	config  *ManagerConfig
	plugins map[string]*pluginInstance[C]
	stop    chan struct{}
	done    chan struct{}
}

func NewManager[C any](name string, config *ManagerConfig) *Manager[C] {
	if config.RestartConfig.MaxRestarts == 0 {
		config.RestartConfig.MaxRestarts = 5
	}
	if config.RestartConfig.PingInterval == 0 {
		config.RestartConfig.PingInterval = 10 * time.Second
	}
	if config.Logger == nil {
		config.Logger = hclog.New(&hclog.LoggerOptions{
			Name:   "plugin-manager",
			Output: os.Stdout,
			Level:  hclog.Debug,
		})
	}

	killed := make(chan PluginInfo, 1)
	m := &Manager[C]{
		Name:    name,
		config:  config,
		plugins: make(map[string]*pluginInstance[C]),
		killed:  killed,
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	if m.config.RestartConfig.Managed {
		go m.supervisor()
	}
	return m
}

func (m *Manager[C]) PluginKilled() <-chan PluginInfo {
	return m.killed
}

func (m *Manager[C]) supervisor() {
	defer close(m.done)

	for {
		select {
		case pm := <-m.killed:
			if pm.Restarts >= m.config.RestartConfig.MaxRestarts {
				m.config.Logger.Error(
					"plugin %v restarts %v exceeded max restarts %v",
					pm.Key,
					pm.Restarts,
					m.config.RestartConfig.MaxRestarts,
				)
				continue
			}
			m.RestartPlugin(PluginInfo{Key: pm.Key, BinPath: pm.BinPath, Checksum: pm.Checksum})
		case <-m.stop:
			return
		}
	}

}

func (m *Manager[C]) Shutdown() error {
	close(m.killed)
	for _, p := range m.plugins {
		p.Stop()
	}
	if m.config.RestartConfig.Managed {
		close(m.stop)
		<-m.done
	}

	return nil
}

func (m *Manager[C]) loadPlugin(pm PluginInfo) (*pluginInstance[C], error) {
	config := &goplugin.ClientConfig{
		HandshakeConfig: m.config.HandshakeConfig,
		Plugins: map[string]goplugin.Plugin{
			m.Name: m.config.Plugin,
		},
		Cmd: exec.Command(pm.BinPath),
	}
	if pm.Checksum != "" {
		src := []byte(pm.Checksum)
		dst := make([]byte, hex.DecodedLen(len(src)))
		_, err := hex.Decode(dst, src)
		if err != nil {
			m.config.Logger.Error(err.Error())
			return nil, err
		}
		config.SecureConfig = &goplugin.SecureConfig{
			Checksum: dst,
			Hash:     sha256.New(),
		}
	}
	client := goplugin.NewClient(config)

	rpcClient, err := client.Client()
	if err != nil {
		m.config.Logger.Error(err.Error())
		return nil, err
	}

	raw, err := rpcClient.Dispense(m.Name)
	if err != nil {
		m.config.Logger.Error(err.Error())
		return nil, err
	}

	impl, ok := raw.(C)
	if !ok {
		return nil, fmt.Errorf("plugin does not implement interface")
	}

	stop, done := make(chan struct{}), make(chan struct{})
	p := &pluginInstance[C]{
		Impl:      impl,
		client:    client,
		rpcClient: rpcClient,
		stop:      stop,
		done:      done,
		Info:      pm,
	}
	go p.Watch(m.config.Logger, m.config.RestartConfig.PingInterval, m.killed)

	return p, nil
}

func (m *Manager[C]) LoadPlugins(plugins []PluginInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, pm := range plugins {
		p, err := m.loadPlugin(pm)
		if err != nil {
			return err
		}
		m.plugins[pm.Key] = p
	}

	return nil
}

func (m *Manager[c]) StopPlugin(pm PluginInfo) error {
	p, ok := m.getPlugin(pm.Key)
	if !ok {
		return fmt.Errorf("plugin %v not found", pm.Key)
	}

	p.Stop()

	err := m.deletePlugin(pm.Key)
	if err != nil {
		m.config.Logger.Error("failed to delete plugin: %v", err)
		return err
	}

	return nil
}

func (m *Manager[C]) StartPlugin(pm PluginInfo) (*pluginInstance[C], error) {
	p, err := m.loadPlugin(pm)
	if err != nil {
		return nil, err
	}

	err = m.insertPlugin(pm.Key, p)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func (m *Manager[C]) RestartPlugin(pm PluginInfo) error {
	restartCount := 0
	p, ok := m.getPlugin(pm.Key)
	if ok {
		restartCount = p.Info.Restarts
	}

	err := m.StopPlugin(pm)
	if err != nil {
		return err
	}

	p, err = m.StartPlugin(pm)
	if err != nil {
		return err
	}

	p.Info.Restarts = restartCount + 1

	m.config.Logger.Debug("restarted plugin: %v", pm)
	return nil
}

func (m *Manager[C]) ListPlugins() ([]PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	metas := []PluginInfo{}
	for _, p := range m.plugins {
		metas = append(metas, p.Info)
	}
	return metas, nil
}

func (m *Manager[C]) GetPlugin(pluginKey string) (C, error) {
	p, ok := m.getPlugin(pluginKey)
	if !ok {
		return *new(C), fmt.Errorf("plugin %v not found", pluginKey)
	}
	return p.Impl, nil
}

func (m *Manager[C]) getPlugin(pluginKey string) (*pluginInstance[C], bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[pluginKey]
	return p, ok
}

func (m *Manager[C]) insertPlugin(pluginKey string, p *pluginInstance[C]) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plugins[pluginKey] = p
	return nil
}

func (m *Manager[C]) deletePlugin(pluginKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.plugins, pluginKey)
	return nil
}

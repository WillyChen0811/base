// Package omlxgw 是 oMLX 的统一入口网关：透明反向代理 + 并发/优先级调度。
package omlxgw

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

// ModelRule 是单个模型的并发规则。
type ModelRule struct {
	MaxConcurrent int  `json:"max_concurrent"`
	Exclusive     bool `json:"exclusive"`
}

// Config 是网关配置。listen、upstream 启动后不可热改。
type Config struct {
	Listen          string               `json:"listen"`
	Upstream        string               `json:"upstream"`
	DefaultPriority int                  `json:"default_priority"`
	Models          map[string]ModelRule `json:"models"`
	DefaultModel    ModelRule            `json:"default_model"`
	MaxTotal        int                  `json:"max_total"`
	Wake            *WakeConfig          `json:"wake,omitempty"`
	KeepAwake       *KeepAwakeConfig     `json:"keepawake,omitempty"`
}

// WakeConfig 控制上游睡着时的唤醒（省略则关闭）。
type WakeConfig struct {
	Host           string `json:"host"`
	KnockPorts     []int  `json:"knock_ports"`
	MaxWaitSeconds int    `json:"max_wait_seconds"`
}

// KeepAwakeConfig 控制有活时的防睡命令（省略则关闭）。
type KeepAwakeConfig struct {
	Command        []string `json:"command"`
	RefreshSeconds int      `json:"refresh_seconds"`
	IdleSeconds    int      `json:"idle_seconds"`
}

func (c *Config) applyDefaults() {
	if c.DefaultModel.MaxConcurrent == 0 {
		c.DefaultModel.MaxConcurrent = 8
	}
	if c.MaxTotal == 0 {
		c.MaxTotal = 16
	}
	if c.Wake != nil && c.Wake.MaxWaitSeconds == 0 {
		c.Wake.MaxWaitSeconds = 120
	}
	if k := c.KeepAwake; k != nil {
		if k.RefreshSeconds == 0 {
			k.RefreshSeconds = 240
		}
		if k.IdleSeconds == 0 {
			k.IdleSeconds = 600
		}
	}
	if c.Models == nil {
		c.Models = map[string]ModelRule{}
	}
}

// Validate 校验配置。
func (c *Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen 无效: %v", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("listen 只允许回环地址")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream 无效")
	}
	if c.DefaultPriority < 0 || c.DefaultPriority > 9 {
		return fmt.Errorf("default_priority 须在 0-9")
	}
	if c.MaxTotal < 1 {
		return fmt.Errorf("max_total 须 >= 1")
	}
	if c.DefaultModel.MaxConcurrent < 1 {
		return fmt.Errorf("default_model.max_concurrent 须 >= 1")
	}
	for name, r := range c.Models {
		if r.MaxConcurrent < 1 {
			return fmt.Errorf("models[%s].max_concurrent 须 >= 1", name)
		}
	}
	if w := c.Wake; w != nil {
		if w.Host == "" {
			return fmt.Errorf("wake.host 不能为空")
		}
		for _, p := range w.KnockPorts {
			if p < 1 || p > 65535 {
				return fmt.Errorf("wake.knock_ports 含无效端口 %d", p)
			}
		}
		if w.MaxWaitSeconds < 1 {
			return fmt.Errorf("wake.max_wait_seconds 须 >= 1")
		}
	}
	if k := c.KeepAwake; k != nil {
		if len(k.Command) == 0 || k.Command[0] == "" {
			return fmt.Errorf("keepawake.command 不能为空")
		}
		if k.RefreshSeconds < 1 || k.IdleSeconds < 1 {
			return fmt.Errorf("keepawake.refresh_seconds、idle_seconds 须 >= 1")
		}
	}
	return nil
}

func (c *Config) rule(model string) ModelRule {
	if r, ok := c.Models[model]; ok {
		return r
	}
	return c.DefaultModel
}

// LoadConfig 读取并校验配置文件。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig 解析 JSON（拒绝未知字段）、补缺省并校验。
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("配置不是合法 JSON: %v", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// writeFileAtomic 先写临时文件再 rename。
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".omlxgw-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

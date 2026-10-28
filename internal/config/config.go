// Package config 负责配置与数据文件的路径解析、配置读写。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	// DefaultAddr 是网关默认监听地址，只绑回环，不对外。
	DefaultAddr = "127.0.0.1:3425"

	dirName = "switchboard"
)

// Config 是磁盘上的配置文件内容。账号与路由不放这里，放在数据库。
type Config struct {
	Addr string `json:"addr"`
	DB   string `json:"db"`
}

// Paths 是一次运行里用到的两个文件位置。
type Paths struct {
	Config string
	DB     string
}

// Resolve 按覆盖参数与 XDG 约定算出文件位置。
func Resolve(configOverride, dbOverride string) (Paths, error) {
	var p Paths

	cfgDir, err := configDir()
	if err != nil {
		return p, err
	}
	dataDirPath, err := dataDir()
	if err != nil {
		return p, err
	}

	p.Config = filepath.Join(cfgDir, "config.json")
	p.DB = filepath.Join(dataDirPath, "switchboard.db")
	if configOverride != "" {
		p.Config = configOverride
	}
	if dbOverride != "" {
		p.DB = dbOverride
	}
	return p, nil
}

// Default 返回首次初始化写入的配置。
func Default(p Paths) Config {
	return Config{Addr: DefaultAddr, DB: p.DB}
}

// Load 读取配置。字段缺失时补默认值，不视为错误。
func Load(p Paths) (Config, error) {
	cfg := Config{Addr: DefaultAddr, DB: p.DB}

	b, err := os.ReadFile(p.Config)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.DB == "" {
		cfg.DB = p.DB
	}
	return cfg, nil
}

// Save 写入配置，权限 0600。
func Save(p Paths, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(p.Config), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.Config, b, 0o600)
}

func configDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", dirName), nil
}

func dataDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", dirName), nil
}

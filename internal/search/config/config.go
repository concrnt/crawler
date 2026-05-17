package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	envConfigPath     = "CONCRNT_SEARCH_CONFIG"
	defaultConfigPath = "/etc/concrnt-search/config.yaml"
	localConfigPath   = "config.local.yaml"
	rootConfigPath    = "config.yaml"
)

const (
	DefaultProfileSchema   = "https://schema.concrnt.world/p/main.json"
	DefaultCommunitySchema = "https://schema.concrnt.world/t/community.json"
)

type Duration time.Duration

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if value.Value == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

type Config struct {
	Server        Server        `yaml:"server"`
	Crawl         Crawl         `yaml:"crawl"`
	Backends      Backends      `yaml:"backends"`
	Observability Observability `yaml:"observability"`
}

type Server struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"publicURL"`
}

type Crawl struct {
	Seed                 string   `yaml:"seed"`
	Prefix               string   `yaml:"prefix"`
	KnownServersInterval Duration `yaml:"knownServersInterval"`
	IncrementalInterval  Duration `yaml:"incrementalInterval"`
	RequestTimeout       Duration `yaml:"requestTimeout"`
	GlobalConcurrency    int      `yaml:"globalConcurrency"`
	PerServerConcurrency int      `yaml:"perServerConcurrency"`
	PageLimit            int      `yaml:"pageLimit"`
	Overlap              Duration `yaml:"overlap"`
	MaxPagesPerRun       int      `yaml:"maxPagesPerRun"`
	ProfileSchemas       []string `yaml:"profileSchemas"`
	CommunitySchemas     []string `yaml:"communitySchemas"`
}

type Backends struct {
	PostgresDsn string `yaml:"postgresDsn"`
	MeiliHost   string `yaml:"meiliHost"`
	MeiliAPIKey string `yaml:"meiliAPIKey"`
}

type Observability struct {
	EnableTrace   bool   `yaml:"enableTrace"`
	TraceEndpoint string `yaml:"traceEndpoint"`
}

func Default() Config {
	return Config{
		Server: Server{
			Listen: ":8080",
		},
		Crawl: Crawl{
			Prefix:               "cckv://",
			KnownServersInterval: Duration(10 * time.Minute),
			IncrementalInterval:  Duration(15 * time.Minute),
			RequestTimeout:       Duration(10 * time.Second),
			GlobalConcurrency:    8,
			PerServerConcurrency: 1,
			PageLimit:            100,
			Overlap:              Duration(2 * time.Minute),
			MaxPagesPerRun:       1000,
			ProfileSchemas:       []string{DefaultProfileSchema},
			CommunitySchemas:     []string{DefaultCommunitySchema},
		},
		Backends: Backends{
			MeiliHost: "http://meilisearch:7700",
		},
	}
}

func LoadFromEnv() (Config, error) {
	path := os.Getenv(envConfigPath)
	if path == "" {
		if _, err := os.Stat(localConfigPath); err == nil {
			return Load(localConfigPath)
		}
		if _, err := os.Stat(rootConfigPath); err == nil {
			return Load(rootConfigPath)
		}
		path = defaultConfigPath
	}
	return Load(path)
}

func Load(path string) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()

	if err := yaml.NewDecoder(file).Decode(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Crawl.Seed == "" {
		return fmt.Errorf("crawl.seed is required")
	}
	if c.Crawl.Prefix == "" {
		c.Crawl.Prefix = "cckv://"
	}
	if c.Crawl.KnownServersInterval.Duration() <= 0 {
		c.Crawl.KnownServersInterval = Duration(10 * time.Minute)
	}
	if c.Crawl.IncrementalInterval.Duration() <= 0 {
		c.Crawl.IncrementalInterval = Duration(15 * time.Minute)
	}
	if c.Crawl.RequestTimeout.Duration() <= 0 {
		c.Crawl.RequestTimeout = Duration(10 * time.Second)
	}
	if c.Crawl.GlobalConcurrency <= 0 {
		c.Crawl.GlobalConcurrency = 1
	}
	if c.Crawl.PerServerConcurrency <= 0 {
		c.Crawl.PerServerConcurrency = 1
	}
	if c.Crawl.PageLimit <= 0 {
		c.Crawl.PageLimit = 100
	}
	if c.Crawl.PageLimit > 100 {
		c.Crawl.PageLimit = 100
	}
	if c.Crawl.Overlap.Duration() < 0 {
		return fmt.Errorf("crawl.overlap must not be negative")
	}
	if c.Crawl.MaxPagesPerRun <= 0 {
		c.Crawl.MaxPagesPerRun = 1000
	}
	if len(c.Crawl.ProfileSchemas) == 0 {
		c.Crawl.ProfileSchemas = []string{DefaultProfileSchema}
	}
	if len(c.Crawl.CommunitySchemas) == 0 {
		c.Crawl.CommunitySchemas = []string{DefaultCommunitySchema}
	}
	if c.Backends.PostgresDsn == "" {
		return fmt.Errorf("backends.postgresDsn is required")
	}
	if c.Backends.MeiliHost == "" {
		return fmt.Errorf("backends.meiliHost is required")
	}
	return nil
}

package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	envConfigPath     = "CONCRNT_CRAWLER_CONFIG"
	defaultConfigPath = "/etc/concrnt-crawler/config.yaml"
	localConfigPath   = "config.local.yaml"
	rootConfigPath    = "config.yaml"
)

const (
	DefaultProfileSchema   = "https://schema.concrnt.world/p/main.json"
	DefaultCommunitySchema = "https://schema.concrnt.world/t/community.json"
)

// DefaultPostSchemas are the world message schemas indexed as posts.
var DefaultPostSchemas = []string{
	"https://schema.concrnt.world/m/markdown.json",
	"https://schema.concrnt.world/m/reply.json",
	"https://schema.concrnt.world/m/reroute.json",
	"https://schema.concrnt.world/m/plaintext.json",
	"https://schema.concrnt.world/m/media.json",
	"https://schema.concrnt.world/m/gfm.json",
	"https://schema.concrnt.world/m/mfm.json",
	"https://schema.concrnt.world/m/cfm.json",
}

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
	Layer                string   `yaml:"layer"`
	KnownServersInterval Duration `yaml:"knownServersInterval"`
	IncrementalInterval  Duration `yaml:"incrementalInterval"`
	RequestTimeout       Duration `yaml:"requestTimeout"`
	GlobalConcurrency    int      `yaml:"globalConcurrency"`
	PerServerConcurrency int      `yaml:"perServerConcurrency"`
	PageLimit            int      `yaml:"pageLimit"`
	Overlap              Duration `yaml:"overlap"`
	MaxPagesPerRun       int      `yaml:"maxPagesPerRun"`
	ActivityInterval     Duration `yaml:"activityInterval"`
	ActivityHalfLife     Duration `yaml:"activityHalfLife"`
	ActivityHistoryDays  int      `yaml:"activityHistoryDays"`
	ProfileSchemas       []string `yaml:"profileSchemas"`
	CommunitySchemas     []string `yaml:"communitySchemas"`
	PostSchemas          []string `yaml:"postSchemas"`
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
			KnownServersInterval: Duration(10 * time.Minute),
			IncrementalInterval:  Duration(15 * time.Minute),
			RequestTimeout:       Duration(10 * time.Second),
			GlobalConcurrency:    8,
			PerServerConcurrency: 1,
			PageLimit:            100,
			Overlap:              Duration(10 * time.Second),
			MaxPagesPerRun:       1000,
			ActivityInterval:     Duration(10 * time.Minute),
			ActivityHalfLife:     Duration(7 * 24 * time.Hour),
			ActivityHistoryDays:  30,
			ProfileSchemas:       []string{DefaultProfileSchema},
			CommunitySchemas:     []string{DefaultCommunitySchema},
			PostSchemas:          append([]string(nil), DefaultPostSchemas...),
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
	c.Crawl.Layer = strings.TrimSpace(c.Crawl.Layer)
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
	if c.Crawl.ActivityInterval.Duration() <= 0 {
		c.Crawl.ActivityInterval = Duration(10 * time.Minute)
	}
	if c.Crawl.ActivityHalfLife.Duration() <= 0 {
		c.Crawl.ActivityHalfLife = Duration(7 * 24 * time.Hour)
	}
	if c.Crawl.ActivityHistoryDays <= 0 {
		c.Crawl.ActivityHistoryDays = 30
	}
	if len(c.Crawl.ProfileSchemas) == 0 {
		c.Crawl.ProfileSchemas = []string{DefaultProfileSchema}
	}
	if len(c.Crawl.CommunitySchemas) == 0 {
		c.Crawl.CommunitySchemas = []string{DefaultCommunitySchema}
	}
	if len(c.Crawl.PostSchemas) == 0 {
		c.Crawl.PostSchemas = append([]string(nil), DefaultPostSchemas...)
	}
	if c.Backends.PostgresDsn == "" {
		return fmt.Errorf("backends.postgresDsn is required")
	}
	if c.Backends.MeiliHost == "" {
		return fmt.Errorf("backends.meiliHost is required")
	}
	return nil
}

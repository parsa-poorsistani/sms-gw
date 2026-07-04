package configs

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Postgres   PostgresConfig   `mapstructure:"postgres"`
	Dispatcher DispatcherConfig `mapstructure:"dispatcher"`
	Provider   ProviderConfig   `mapstructure:"provider"`
	Metrics    MetricsConfig    `mapstructure:"metrics"`
}

type ServerConfig struct {
	ListenAddr        string        `mapstructure:"listen_addr"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
}

type PostgresConfig struct {
	Host               string        `mapstructure:"host"`
	Port               int           `mapstructure:"port"`
	User               string        `mapstructure:"user"`
	Password           string        `mapstructure:"password"`
	DBName             string        `mapstructure:"dbname"`
	SSLMode            string        `mapstructure:"sslmode"`
	DatabaseURL        string        `mapstructure:"database_url"`
	MaxOpenConnections int           `mapstructure:"max_open_connections"`
	MaxIdleConnections int           `mapstructure:"max_idle_connections"`
	ConnMaxLifetime    time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime    time.Duration `mapstructure:"conn_max_idle_time"`
	MigrationsPath     string        `mapstructure:"migrations_path"`
}

func (c *PostgresConfig) DSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.DBName, c.SSLMode,
	)
}

type DispatcherConfig struct {
	StandardWorkers  int           `mapstructure:"standard_workers"`
	ExpressWorkers   int           `mapstructure:"express_workers"`
	BatchSize        int           `mapstructure:"batch_size"`
	ExpressBatchSize int           `mapstructure:"express_batch_size"`
	PollInterval     time.Duration `mapstructure:"poll_interval"`
	MaxAttempts      int           `mapstructure:"max_attempts"`
	SendTimeout      time.Duration `mapstructure:"send_timeout"`
	// ClaimTimeout is the claim lease: messages in 'sending' older than this
	// are presumed orphaned by a dead worker and requeued by the janitor.
	ClaimTimeout    time.Duration `mapstructure:"claim_timeout"`
	JanitorInterval time.Duration `mapstructure:"janitor_interval"`
}

type ProviderConfig struct {
	Latency     time.Duration `mapstructure:"latency"`
	FailureRate float64       `mapstructure:"failure_rate"`
	// LatencySchedule ("0s:50ms,330s:200ms,420s:50ms") makes the mock
	// operator's latency vary over the run — brownout simulation for load
	// tests. Empty = fixed Latency.
	LatencySchedule string `mapstructure:"latency_schedule"`
}

type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
}

func Load() (*Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./configs")
	v.AddConfigPath(".")

	v.SetEnvPrefix("SMS_GW")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	for _, key := range []string{
		"postgres.user",
		"postgres.password",
		"postgres.database_url",
		"provider.latency_schedule",
	} {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("bind env %s: %w", key, err)
		}
	}

	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.listen_addr", ":8080")
	v.SetDefault("server.read_header_timeout", "5s")
	v.SetDefault("server.shutdown_timeout", "10s")

	v.SetDefault("postgres.host", "localhost")
	v.SetDefault("postgres.port", 5432)
	v.SetDefault("postgres.dbname", "sms")
	v.SetDefault("postgres.sslmode", "disable")
	v.SetDefault("postgres.max_open_connections", 32)
	v.SetDefault("postgres.max_idle_connections", 16)
	v.SetDefault("postgres.conn_max_lifetime", "30m")
	v.SetDefault("postgres.conn_max_idle_time", "5m")
	v.SetDefault("postgres.migrations_path", "migrations")

	v.SetDefault("dispatcher.standard_workers", 16)
	v.SetDefault("dispatcher.express_workers", 8)
	v.SetDefault("dispatcher.batch_size", 100)
	v.SetDefault("dispatcher.express_batch_size", 10)
	v.SetDefault("dispatcher.poll_interval", "200ms")
	v.SetDefault("dispatcher.max_attempts", 5)
	v.SetDefault("dispatcher.send_timeout", "5s")
	v.SetDefault("dispatcher.claim_timeout", "2m")
	v.SetDefault("dispatcher.janitor_interval", "30s")

	v.SetDefault("provider.latency", "50ms")
	v.SetDefault("provider.failure_rate", 0.02)

	v.SetDefault("metrics.enabled", true)
	v.SetDefault("metrics.path", "/metrics")
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/go-viper/mapstructure/v2"
	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 应用程序总配置
type Config struct {
	Conversation    *ConversationConfig    `yaml:"conversation"     json:"conversation"`
	Server          *ServerConfig          `yaml:"server"           json:"server"`
	KnowledgeBase   *KnowledgeBaseConfig   `yaml:"knowledge_base"   json:"knowledge_base"`
	Tenant          *TenantConfig          `yaml:"tenant"           json:"tenant"`
	Auth            *AuthConfig            `yaml:"auth"            json:"auth"`
	Audit           *AuditConfig           `yaml:"audit"           json:"audit"`
	Backup          *BackupConfig          `yaml:"backup"          json:"backup"`
	OIDCAuth        *OIDCAuthConfig        `yaml:"oidc_auth"        json:"oidc_auth"`
	Models          []ModelConfig          `yaml:"models"           json:"models"`
	VectorDatabase  *VectorDatabaseConfig  `yaml:"vector_database"  json:"vector_database"`
	DocReader       *DocReaderConfig       `yaml:"docreader"        json:"docreader"`
	StreamManager   *StreamManagerConfig   `yaml:"stream_manager"   json:"stream_manager"`
	ExtractManager  *ExtractManagerConfig  `yaml:"extract"          json:"extract"`
	WebSearch       *WebSearchConfig       `yaml:"web_search"       json:"web_search"`
	PromptTemplates *PromptTemplatesConfig `yaml:"prompt_templates" json:"prompt_templates"`
	IM              *IMConfig              `yaml:"im"               json:"im"`
	Agent           *AgentConfig           `yaml:"agent"            json:"agent"`
	// FrontendBaseURL is the externally-visible origin of the SPA, used
	// to compose absolute share-link URLs. Empty falls back to a host-
	// relative URL ("/register?token=…") which the SPA then resolves
	// against window.location.origin — fine for typical single-origin
	// deployments. Sourced from FRONTEND_BASE_URL env at startup.
	FrontendBaseURL string `yaml:"frontend_base_url" json:"frontend_base_url"`
}

// AgentConfig represents the global agent settings.
type AgentConfig struct {
	// LLMCallTimeout is the default timeout for a single LLM call in seconds.
	// Default: 120 (standard agents) or 300 (can be overridden by Env).
	LLMCallTimeout int `yaml:"llm_call_timeout" json:"llm_call_timeout"`
	// ToolApprovalTimeoutSeconds is how long the agent waits for human approval on a flagged MCP tool.
	// 0 means default 600 (10 minutes).
	ToolApprovalTimeoutSeconds int `yaml:"tool_approval_timeout_seconds" json:"tool_approval_timeout_seconds"`
}

// IMConfig configures the IM integration service.
// All fields are optional — zero values fall back to built-in defaults so
// existing deployments need no config changes.
type IMConfig struct {
	// Workers is the number of concurrent QA worker goroutines per instance.
	// Default: 5.
	Workers int `yaml:"workers" json:"workers"`
	// GlobalMaxWorkers is the maximum number of QA requests that can execute
	// concurrently across ALL instances. Enforced via a Redis counter; when the
	// global limit is reached, local workers wait until a slot opens.
	// Requires Redis — ignored in single-instance mode.
	// 0 (default) means no global limit.
	GlobalMaxWorkers int `yaml:"global_max_workers" json:"global_max_workers"`
	// MaxQueueSize is the maximum number of pending QA requests per instance.
	// Default: 50.
	MaxQueueSize int `yaml:"max_queue_size" json:"max_queue_size"`
	// MaxPerUser limits how many requests a single user can have queued globally.
	// Default: 3.
	MaxPerUser int `yaml:"max_per_user" json:"max_per_user"`
	// RateLimitWindow is the sliding window duration for per-user rate limiting.
	// Default: 60s.
	RateLimitWindow time.Duration `yaml:"rate_limit_window" json:"rate_limit_window"`
	// RateLimitMax is the maximum number of requests allowed per window per user.
	// Default: 10.
	RateLimitMax int `yaml:"rate_limit_max" json:"rate_limit_max"`
}

// DocReaderConfig configures the document parser client (gRPC or HTTP).
type DocReaderConfig struct {
	// Addr: for gRPC it is the server address (e.g. "localhost:50051"); for HTTP it is the base URL (e.g. "http://localhost:8080").
	Addr string `yaml:"addr" json:"addr"`
	// Transport: "grpc" (default) or "http"
	Transport string `yaml:"transport" json:"transport"`
}

type VectorDatabaseConfig struct {
	Driver string `yaml:"driver" json:"driver"`
}

// ConversationConfig 对话服务配置
type ConversationConfig struct {
	MaxRounds            int            `yaml:"max_rounds"                       json:"max_rounds"`
	KeywordThreshold     float64        `yaml:"keyword_threshold"                json:"keyword_threshold"`
	EmbeddingTopK        int            `yaml:"embedding_top_k"                  json:"embedding_top_k"`
	VectorThreshold      float64        `yaml:"vector_threshold"                 json:"vector_threshold"`
	RerankTopK           int            `yaml:"rerank_top_k"                     json:"rerank_top_k"`
	RerankThreshold      float64        `yaml:"rerank_threshold"                 json:"rerank_threshold"`
	FallbackStrategy     string         `yaml:"fallback_strategy"                json:"fallback_strategy"`
	FallbackResponse     string         `yaml:"fallback_response"                json:"fallback_response"`
	EnableRewrite        bool           `yaml:"enable_rewrite"                   json:"enable_rewrite"`
	EnableQueryExpansion bool           `yaml:"enable_query_expansion"           json:"enable_query_expansion"`
	EnableRerank         bool           `yaml:"enable_rerank"                    json:"enable_rerank"`
	Summary              *SummaryConfig `yaml:"summary"                          json:"summary"`
	// ContextManager holds deployment-wide defaults for the five-layer
	// context architecture (L0-L4 budgeting, smart history compression,
	// retrieval tiering). A tenant's own ContextConfig (DB jsonb) takes
	// precedence field-by-field; this section is the global fallback.
	ContextManager *ContextManagerConfig `yaml:"context_manager"                json:"context_manager"`

	// Prompt template ID fields — resolved to text by backfillConversationDefaults
	FallbackPromptID             string `yaml:"fallback_prompt_id"                json:"fallback_prompt_id"`
	RewritePromptID              string `yaml:"rewrite_prompt_id"                 json:"rewrite_prompt_id"`
	GenerateSessionTitlePromptID string `yaml:"generate_session_title_prompt_id"  json:"generate_session_title_prompt_id"`
	GenerateSummaryPromptID      string `yaml:"generate_summary_prompt_id"        json:"generate_summary_prompt_id"`
	ExtractEntitiesPromptID      string `yaml:"extract_entities_prompt_id"        json:"extract_entities_prompt_id"`
	ExtractRelationshipsPromptID string `yaml:"extract_relationships_prompt_id"   json:"extract_relationships_prompt_id"`
	GenerateQuestionsPromptID    string `yaml:"generate_questions_prompt_id"      json:"generate_questions_prompt_id"`
	MemoryExtractionPromptID     string `yaml:"memory_extraction_prompt_id"       json:"memory_extraction_prompt_id"`
	SessionPrecipitationPromptID string `yaml:"session_precipitation_prompt_id"  json:"session_precipitation_prompt_id"`
	SessionWikiPromptID          string `yaml:"session_wiki_prompt_id"           json:"session_wiki_prompt_id"`

	// Resolved prompt text fields (populated by backfill, not from YAML)
	FallbackPrompt             string `yaml:"-" json:"fallback_prompt"`
	RewritePromptSystem        string `yaml:"-" json:"rewrite_prompt_system"`
	RewritePromptUser          string `yaml:"-" json:"rewrite_prompt_user"`
	GenerateSessionTitlePrompt string `yaml:"-" json:"generate_session_title_prompt"`
	GenerateSummaryPrompt      string `yaml:"-" json:"generate_summary_prompt"`
	ExtractEntitiesPrompt      string `yaml:"-" json:"extract_entities_prompt"`
	ExtractRelationshipsPrompt string `yaml:"-" json:"extract_relationships_prompt"`
	GenerateQuestionsPrompt    string `yaml:"-" json:"generate_questions_prompt"`
	MemoryExtractionPrompt     string `yaml:"-" json:"memory_extraction_prompt"`
	SessionPrecipitationPrompt string `yaml:"-" json:"session_precipitation_prompt"`
	SessionWikiPrompt          string `yaml:"-" json:"session_wiki_prompt"`

	// IntentSystemPrompts maps intent values (e.g. "greeting", "chitchat") to
	// system prompt text. Populated by backfill from IntentPrompts templates.
	IntentSystemPrompts map[string]string `yaml:"-" json:"-"`
}

// ContextManagerConfig holds global defaults for the five-layer context
// architecture. Fields mirror types.ContextConfig; kept as a separate struct
// so config stays YAML-only and types stays persistence-only.
type ContextManagerConfig struct {
	// MaxTokens is the explicit context window budget. 0 means auto-resolve
	// from the chat model's vendor metadata.
	MaxTokens int `yaml:"max_tokens"          json:"max_tokens"`
	// CompressionStrategy: "sliding_window" (default) or "smart" (five-layer
	// budgeting + Map-Reduce history compression + retrieval tiering).
	CompressionStrategy string `yaml:"compression_strategy" json:"compression_strategy"`
	// RecentMessageCount is the number of recent rounds kept verbatim under
	// the smart strategy (default: max_rounds).
	RecentMessageCount int `yaml:"recent_message_count" json:"recent_message_count"`
	// SummarizeThreshold is the deep fetch window for smart compression —
	// rounds beyond RecentMessageCount up to this limit are Map-Reduce
	// summarized (default: 3x the recent window).
	SummarizeThreshold int `yaml:"summarize_threshold"  json:"summarize_threshold"`
}

// SummaryConfig 摘要配置
type SummaryConfig struct {
	MaxInputChars       int     `yaml:"max_input_chars"       json:"max_input_chars"` // Max input characters for summary generation (default: 16384)
	MaxTokens           int     `yaml:"max_tokens"            json:"max_tokens"`
	RepeatPenalty       float64 `yaml:"repeat_penalty"        json:"repeat_penalty"`
	TopK                int     `yaml:"top_k"                 json:"top_k"`
	TopP                float64 `yaml:"top_p"                 json:"top_p"`
	FrequencyPenalty    float64 `yaml:"frequency_penalty"     json:"frequency_penalty"`
	PresencePenalty     float64 `yaml:"presence_penalty"      json:"presence_penalty"`
	Temperature         float64 `yaml:"temperature"           json:"temperature"`
	Seed                int     `yaml:"seed"                  json:"seed"`
	MaxCompletionTokens int     `yaml:"max_completion_tokens" json:"max_completion_tokens"`
	NoMatchPrefix       string  `yaml:"no_match_prefix"       json:"no_match_prefix"`
	Thinking            *bool   `yaml:"thinking"              json:"thinking"`

	// Prompt template ID fields — resolved to text by backfillConversationDefaults
	PromptID          string `yaml:"prompt_id"           json:"prompt_id"`
	ContextTemplateID string `yaml:"context_template_id" json:"context_template_id"`

	// Resolved prompt text fields (populated by backfill, not from YAML)
	Prompt          string `yaml:"-" json:"prompt"`
	ContextTemplate string `yaml:"-" json:"context_template"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port            int           `yaml:"port"             json:"port"`
	Host            string        `yaml:"host"             json:"host"`
	LogPath         string        `yaml:"log_path"         json:"log_path"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout" default:"30s"`
}

// KnowledgeBaseConfig 知识库配置
type KnowledgeBaseConfig struct {
	ChunkSize              int                    `yaml:"chunk_size"       json:"chunk_size"`
	ChunkOverlap           int                    `yaml:"chunk_overlap"    json:"chunk_overlap"`
	SplitMarkers           []string               `yaml:"split_markers"    json:"split_markers"`
	KeepSeparator          bool                   `yaml:"keep_separator"   json:"keep_separator"`
	ImageProcessing        *ImageProcessingConfig `yaml:"image_processing" json:"image_processing"`
	DocumentProcessTimeout time.Duration          `yaml:"document_process_timeout"  json:"document_process_timeout"`
	// DocReaderCallTimeout caps a single DocReader RPC. Without this the
	// gRPC call inherits the asynq task context (whole DocumentProcessTimeout,
	// default 2h+), so a hung docreader would block a worker for hours and
	// leave knowledge in "processing". Default 30 minutes is generous enough
	// for OCR-heavy large PDFs while ensuring forward progress.
	DocReaderCallTimeout time.Duration `yaml:"docreader_call_timeout"   json:"docreader_call_timeout"`
}

// DefaultDocumentProcessTimeout is the ceiling for a single document:process
// Asynq task when document_process_timeout is unset or non-positive.
const DefaultDocumentProcessTimeout = 2 * time.Hour

// DocumentProcessTimeout returns the effective document-process task timeout.
// Partial configs (e.g. unit tests) receive the default when unset.
func DocumentProcessTimeout(cfg *Config) time.Duration {
	if cfg != nil && cfg.KnowledgeBase != nil && cfg.KnowledgeBase.DocumentProcessTimeout > 0 {
		return cfg.KnowledgeBase.DocumentProcessTimeout
	}
	return DefaultDocumentProcessTimeout
}

// ImageProcessingConfig 图像处理配置
type ImageProcessingConfig struct {
	EnableMultimodal bool `yaml:"enable_multimodal" json:"enable_multimodal"`
}

// TenantConfig 空间配置
type TenantConfig struct {
	DefaultSessionName        string `yaml:"default_session_name"        json:"default_session_name"`
	DefaultSessionTitle       string `yaml:"default_session_title"       json:"default_session_title"`
	DefaultSessionDescription string `yaml:"default_session_description" json:"default_session_description"`
	// EnableCrossTenantAccess enables cross-tenant access for users with permission
	EnableCrossTenantAccess bool `yaml:"enable_cross_tenant_access" json:"enable_cross_tenant_access"`
	// EnableRBAC turns on tenant-level role enforcement (issue #1303).
	// Pointer so we can distinguish "unset" from "explicit false":
	//   nil           — fall back to the built-in default (true) applied
	//                   by applyAuthAndTenantDefaults.
	//   pointer false — operators opted into the logging-only rollout
	//                   window (set via config.yaml `enable_rbac: false`
	//                   or env `WEKNORA_TENANT_ENABLE_RBAC=false`).
	//   pointer true  — enforcement on (the new default).
	// Read through IsRBACEnforced so callers stay nil-safe.
	EnableRBAC *bool `yaml:"enable_rbac" json:"enable_rbac"`
	// MaxOwnedPerUser caps how many tenants a single non-superuser can
	// create (and Own) via self-service POST /tenants. Counts only Owner
	// memberships so being invited as Admin/Editor/Viewer in another
	// tenant doesn't burn quota. Cross-tenant superusers
	// (CanAccessAllTenants) are exempt.
	//   > 0 — enforce the cap (handler returns 429 when reached).
	//   = 0 — fall back to defaultMaxOwnedTenantsPerUser in the handler.
	//   < 0 — disable the cap entirely (not recommended in shared deployments).
	//
	// Env override: WEKNORA_TENANT_MAX_OWNED_PER_USER (integer). When set
	// and parseable it always wins over config.yaml so operators can
	// loosen / tighten the quota without a redeploy. See
	// applyAuthAndTenantDefaults for the semantics of <0 / 0 / >0.
	MaxOwnedPerUser int `yaml:"max_owned_per_user" json:"max_owned_per_user" mapstructure:"max_owned_per_user"`
	// SelfServiceCreationEnabled controls whether ordinary authenticated
	// users may create a workspace for themselves. Nil preserves the
	// historical default (enabled); cross-tenant superusers are exempt.
	SelfServiceCreationEnabled *bool `yaml:"self_service_creation_enabled" json:"self_service_creation_enabled" mapstructure:"self_service_creation_enabled"`
}

// IsRBACEnforced reports whether tenant-level role enforcement is
// active. Nil receiver or nil EnableRBAC pointer means "operator did
// not opt out", which after applyAuthAndTenantDefaults is the new
// default (true). Callers that need to treat a nil *Config as
// fail-open (legacy behaviour) should keep their own `cfg != nil`
// short-circuit before invoking this helper.
func (t *TenantConfig) IsRBACEnforced() bool {
	if t == nil || t.EnableRBAC == nil {
		return true
	}
	return *t.EnableRBAC
}

// IsSelfServiceCreationEnabled reports whether ordinary users may create
// tenants. Nil keeps the historical behaviour enabled.
func (t *TenantConfig) IsSelfServiceCreationEnabled() bool {
	return t == nil || t.SelfServiceCreationEnabled == nil || *t.SelfServiceCreationEnabled
}

// AuditConfig governs durable audit log behaviour. Writes happen on
// every member-management mutation and on RBAC denials (when
// EnableRBAC is true); the table grows monotonically unless this
// section turns on retention.
type AuditConfig struct {
	// RetentionDays is how many days of audit history to keep. Older
	// rows are deleted by a daily background sweep.
	//   > 0 — purge rows whose created_at < NOW() - retention_days.
	//   = 0 — disable purge entirely (the pre-rollout default).
	//   < 0 — invalid; ValidateConfig rejects it.
	// Default: 90 (set by applyAuditDefaults when the section is omitted).
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
}

// BackupConfig governs the daily user-data backup & recovery subsystem
// (PRD docs/prd/data-backup-recovery.md §7). Everything is optional:
// a nil or disabled section means the scheduler never registers, and the
// backup API surface answers 503.
type BackupConfig struct {
	// Enabled is the master switch. Default false.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Storage is the backup target — deliberately a separate instance
	// from the primary storage so a lost primary does not take the
	// backups with it (PRD §2.2 B5).
	Storage *BackupStorageConfig `yaml:"storage" json:"storage,omitempty"`
	// Schedule is a 6-field cron expression (seconds-first, matching
	// the datasource scheduler). Default "0 0 3 * * *": daily 03:00.
	Schedule string `yaml:"schedule" json:"schedule"`
	// Retention GFS tiers: keep N daily / N weekly / N monthly
	// snapshots. 0 disables that tier's pruning contribution. Defaults
	// 7 / 4 / 6.
	RetentionDaily   int `yaml:"retention_daily"   json:"retention_daily"`
	RetentionWeekly  int `yaml:"retention_weekly"  json:"retention_weekly"`
	RetentionMonthly int `yaml:"retention_monthly" json:"retention_monthly"`
	// Compression: "gzip" (default) or "none". Applies to metadata
	// jsonl streams only; file-tier objects are copied byte-for-byte.
	Compression string `yaml:"compression" json:"compression"`
	// Encrypt enables AES-256-GCM envelope encryption of metadata
	// blobs and the manifest, keyed off SYSTEM_AES_KEY. Default false.
	Encrypt bool `yaml:"encrypt" json:"encrypt"`
	// Concurrency knobs: how many workspaces and how many objects per
	// workspace copy in parallel. Defaults 2 / 8.
	ConcurrencyTenants int `yaml:"concurrency_tenants" json:"concurrency_tenants"`
	ConcurrencyObjects int `yaml:"concurrency_objects" json:"concurrency_objects"`
	// PreDeleteSnapshot: snapshot a workspace right before it is
	// deleted, giving operators an undo window. Default true.
	PreDeleteSnapshot *bool `yaml:"pre_delete_snapshot" json:"pre_delete_snapshot,omitempty"`
}

// BackupStorageConfig describes the backup target backend. Supported
// providers: "local" (a directory on the backup host) and S3-compatible
// object stores via "minio" / "s3" (MinIO, AWS S3, and any endpoint
// speaking the S3 protocol — most clouds offer one).
type BackupStorageConfig struct {
	// Provider: local | minio | s3.
	Provider string `yaml:"provider" json:"provider"`
	// Local path when provider == local.
	LocalPath string `yaml:"local_path" json:"local_path,omitempty"`
	// S3-compatible settings (provider minio|s3).
	Endpoint  string `yaml:"endpoint"  json:"endpoint,omitempty"`
	AccessKey string `yaml:"access_key" json:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key" json:"secret_key,omitempty"`
	Bucket    string `yaml:"bucket"    json:"bucket,omitempty"`
	// UseSSL for the S3-compatible endpoint. Default true when an
	// https endpoint is given.
	UseSSL *bool `yaml:"use_ssl" json:"use_ssl,omitempty"`
	// PathPrefix inside the bucket (default "backups").
	PathPrefix string `yaml:"path_prefix" json:"path_prefix,omitempty"`
}

// IsPreDeleteSnapshotEnabled reports whether workspace deletion should
// trigger a final snapshot. Default true (nil = enabled).
func (b *BackupConfig) IsPreDeleteSnapshotEnabled() bool {
	return b == nil || b.PreDeleteSnapshot == nil || *b.PreDeleteSnapshot
}

// AuthConfig governs the user authentication entry points.
type AuthConfig struct {
	// RegistrationMode controls who may call POST /auth/register.
	//   "self_serve" (default) — anyone may register; a new tenant is
	//                            auto-created and the registrant becomes
	//                            its Owner. Preserves existing behaviour.
	//   "invite_only"          — public registration is rejected; new
	//                            users only enter through the invitation
	//                            flow added in PR 3.
	RegistrationMode string `yaml:"registration_mode" json:"registration_mode"`
	// DefaultTenantMode controls public password-registration provisioning.
	// create_personal preserves the historical one-user-one-workspace default;
	// tenantless creates only the identity and waits for an invitation or an
	// explicit self-service tenant creation.
	DefaultTenantMode string `yaml:"default_tenant_mode" json:"default_tenant_mode"`
	// Captcha configures the human-verification challenge presented on the
	// login/register surfaces (P0-4, docs/prd/auth-dual-channel-verification.md §5).
	Captcha *AuthCaptchaConfig `yaml:"captcha" json:"captcha,omitempty"`
	// VerificationCode configures the SMS/email ownership-proof codes (P0-4 §6).
	VerificationCode *AuthVerificationCodeConfig `yaml:"verification_code" json:"verification_code,omitempty"`
	// SMS configures the short-message channel used by phone registration.
	SMS *AuthSMSConfig `yaml:"sms" json:"sms,omitempty"`
	// Email configures the email channel used by email-code registration.
	Email *AuthEmailCodeConfig `yaml:"email" json:"email,omitempty"`
}

// AuthCaptchaConfig selects the human-verification challenge type and where
// it is enforced.
type AuthCaptchaConfig struct {
	// Type is the challenge flavour: "slider" (default) or "text".
	Type string `yaml:"type" json:"type"`
	// LoginRequired toggles the captcha gate on POST /auth/login. Default
	// true; operators running pure API integrations (headless widgets,
	// scripted clients) may set false to keep those clients working.
	LoginRequired *bool `yaml:"login_required" json:"login_required,omitempty"`
}

// AuthVerificationCodeConfig bounds the ownership-proof codes.
type AuthVerificationCodeConfig struct {
	Length                 int `yaml:"length"                   json:"length"`
	TTLMinutes             int `yaml:"ttl_minutes"              json:"ttl_minutes"`
	ResendIntervalSeconds  int `yaml:"resend_interval_seconds"  json:"resend_interval_seconds"`
	DailyLimitPerTarget    int `yaml:"daily_limit_per_target"   json:"daily_limit_per_target"`
	MaxAttempts            int `yaml:"max_attempts"             json:"max_attempts"`
}

// AuthSMSConfig selects the SMS provider. provider "log" writes the code to
// the server log instead of sending anything (development mode); "aliyun"
// sends via Alibaba Cloud dysmsapi using the ACS3 signature.
type AuthSMSConfig struct {
	Provider string                  `yaml:"provider"  json:"provider"`
	Aliyun   *AuthAliyunSMSConfig    `yaml:"aliyun"    json:"aliyun,omitempty"`
}

// AuthAliyunSMSConfig carries Alibaba Cloud dysmsapi credentials.
type AuthAliyunSMSConfig struct {
	AccessKeyID     string `yaml:"access_key_id"     json:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret" json:"-"`
	SignName        string `yaml:"sign_name"          json:"sign_name"`
	TemplateCode    string `yaml:"template_code"     json:"template_code"`
}

// AuthEmailCodeConfig selects the email provider. provider "log" writes the
// code to the server log (development); "smtp" sends real mail through the
// configured SMTP server (465 implicit SSL or 587/25 STARTTLS).
type AuthEmailCodeConfig struct {
	Provider string         `yaml:"provider" json:"provider"`
	SMTP     *AuthSMTPConfig `yaml:"smtp"     json:"smtp,omitempty"`
}

// AuthSMTPConfig carries the outbound SMTP connection details.
type AuthSMTPConfig struct {
	// Preset names a well-known mailbox provider (qq/163/126/gmail/exmail/
	// aliyun/outlook). When set, host/port/use_ssl fall back to that
	// provider's standard values; explicitly configured fields always win
	// over the preset, so custom enterprise mailboxes can mix preset port
	// with a private host.
	Preset   string `yaml:"preset"   json:"preset"`
	Host     string `yaml:"host"     json:"host"`
	Port     int    `yaml:"port"     json:"port"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
	From     string `yaml:"from"     json:"from"`
	// UseSSL selects implicit TLS (port 465). When false, STARTTLS is
	// attempted on plain connections (587/25).
	UseSSL *bool `yaml:"use_ssl" json:"use_ssl,omitempty"`
}

// smtpPresetDefaults maps well-known mailbox providers to their standard
// outbound SMTP settings. Provider credentials (授权码/应用密码) are always
// account-specific and must be configured separately — presets never carry
// credentials.
var smtpPresetDefaults = map[string]struct {
	host   string
	port   int
	useSSL bool
}{
	"qq":      {"smtp.qq.com", 465, true},
	"163":     {"smtp.163.com", 465, true},
	"126":     {"smtp.126.com", 465, true},
	"gmail":   {"smtp.gmail.com", 465, true},
	"exmail":  {"smtp.exmail.qq.com", 465, true},  // 腾讯企业邮箱
	"aliyun":  {"smtp.qiye.aliyun.com", 465, true}, // 阿里企业邮箱
	"outlook": {"smtp.office365.com", 587, false},  // Microsoft 365 / Outlook（STARTTLS）
}

// applySMTPPresetDefaults fills host/port/use_ssl from the named preset
// when those fields were not set explicitly (yaml or env). An explicit
// value always wins over the preset.
func applySMTPPresetDefaults(smtp *AuthSMTPConfig) {
	if smtp == nil {
		return
	}
	preset, ok := smtpPresetDefaults[strings.ToLower(strings.TrimSpace(smtp.Preset))]
	if !ok {
		return
	}
	if strings.TrimSpace(smtp.Host) == "" {
		smtp.Host = preset.host
	}
	if smtp.Port == 0 {
		smtp.Port = preset.port
	}
	if smtp.UseSSL == nil {
		useSSL := preset.useSSL
		smtp.UseSSL = &useSSL
	}
}

// AuthRegistrationMode constants used by handlers and middleware.
const (
	AuthRegistrationModeSelfServe       = "self_serve"
	AuthRegistrationModeInviteOnly      = "invite_only"
	AuthDefaultTenantModeCreatePersonal = "create_personal"
	AuthDefaultTenantModeTenantless     = "tenantless"
)

// IsInviteOnly returns true when registration is gated behind invitations.
// Treats nil receiver and empty/unknown values as "not invite-only" so the
// default keeps current behaviour even if the section is missing from the
// config file.
func (c *AuthConfig) IsInviteOnly() bool {
	if c == nil {
		return false
	}
	return c.RegistrationMode == AuthRegistrationModeInviteOnly
}

type OIDCUserInfoMapping struct {
	Username string `yaml:"username" json:"username"`
	Email    string `yaml:"email"    json:"email"`
}

type OIDCAuthConfig struct {
	Enable                bool                 `yaml:"enable"                 json:"enable"`
	IssuerURL             string               `yaml:"issuer_url"             json:"issuer_url"`
	DiscoveryURL          string               `yaml:"discovery_url"          json:"discovery_url"`
	ProviderDisplayName   string               `yaml:"provider_display_name"  json:"provider_display_name"`
	ClientID              string               `yaml:"client_id"              json:"client_id"`
	ClientSecret          string               `yaml:"client_secret"          json:"-"`
	AuthorizationEndpoint string               `yaml:"authorization_endpoint" json:"authorization_endpoint"`
	TokenEndpoint         string               `yaml:"token_endpoint"         json:"token_endpoint"`
	UserInfoEndpoint      string               `yaml:"user_info_endpoint"     json:"user_info_endpoint"`
	Scopes                []string             `yaml:"scopes"                 json:"scopes"`
	UserInfoMapping       *OIDCUserInfoMapping `yaml:"user_info_mapping"      json:"user_info_mapping"`
}

// PromptTemplateI18n holds localized name and description for a prompt template.
type PromptTemplateI18n struct {
	Name        string `yaml:"name"        json:"name"`
	Description string `yaml:"description" json:"description"`
}

// PromptTemplate 提示词模板
//
// 字段设计：每个模板最多由两部分组成 —— 系统侧 (content) 和用户侧 (user)。
//   - content: 主要内容 / 系统 Prompt（所有模板都使用此字段）
//   - user:    用户侧 Prompt（仅在需要 system+user 配对的模板中使用，如 rewrite、keywords_extraction）
//   - i18n:    多语言 name/description，键为 locale（如 "zh-CN"、"en-US"、"ko-KR"），后端根据请求语言替换 Name/Description 再返回
type PromptTemplate struct {
	ID               string                        `yaml:"id"                 json:"id"`
	Name             string                        `yaml:"name"               json:"name"`
	Description      string                        `yaml:"description"        json:"description"`
	Content          string                        `yaml:"content"            json:"content"`
	User             string                        `yaml:"user"               json:"user,omitempty"`
	HasKnowledgeBase bool                          `yaml:"has_knowledge_base" json:"has_knowledge_base,omitempty"`
	HasWebSearch     bool                          `yaml:"has_web_search"     json:"has_web_search,omitempty"`
	Default          bool                          `yaml:"default"            json:"default,omitempty"`
	Mode             string                        `yaml:"mode"               json:"mode,omitempty"`
	I18n             map[string]PromptTemplateI18n `yaml:"i18n"               json:"-"`
}

// PromptTemplatesConfig 提示词模板配置
//
// 每种 Prompt 类型对应一个 YAML 文件，所有模板都在同一个字段（文件）中管理。
// 每个模板使用 content (system prompt) + user (user prompt) 两个字段。
type PromptTemplatesConfig struct {
	SystemPrompt    []PromptTemplate `yaml:"system_prompt"    json:"system_prompt"`
	ContextTemplate []PromptTemplate `yaml:"context_template" json:"context_template"`
	// Rewrite 合并了前端可选模板和运行时默认模板，每个模板同时包含 content + user
	Rewrite []PromptTemplate `yaml:"rewrite" json:"rewrite"`
	// Fallback 合并了固定回复模板和模型兜底 prompt（通过 mode:"model" 区分）
	Fallback []PromptTemplate `yaml:"fallback" json:"fallback"`

	GenerateSessionTitle []PromptTemplate `yaml:"generate_session_title" json:"generate_session_title,omitempty"`
	GenerateSummary      []PromptTemplate `yaml:"generate_summary"       json:"generate_summary,omitempty"`
	KeywordsExtraction   []PromptTemplate `yaml:"keywords_extraction"    json:"keywords_extraction,omitempty"`
	AgentSystemPrompt    []PromptTemplate `yaml:"agent_system_prompt"    json:"agent_system_prompt,omitempty"`
	GraphExtraction      []PromptTemplate `yaml:"graph_extraction"       json:"graph_extraction,omitempty"`
	MemoryExtraction     []PromptTemplate `yaml:"memory_extraction"      json:"memory_extraction,omitempty"`
	// SessionPrecipitation holds the 知识沉淀 (4.4) templates: distilling
	// high-value sessions into knowledge documents and wiki articles.
	SessionPrecipitation []PromptTemplate `yaml:"session_precipitation"  json:"session_precipitation,omitempty"`
	GenerateQuestions    []PromptTemplate `yaml:"generate_questions"     json:"generate_questions,omitempty"`
	// IntentPrompts holds per-intent system prompt overrides (template ID = intent value).
	IntentPrompts []PromptTemplate `yaml:"intent_prompts" json:"intent_prompts,omitempty"`
}

// DefaultTemplate returns the first template marked as default in the list,
// or the first template if none is marked, or nil if the list is empty.
func DefaultTemplate(templates []PromptTemplate) *PromptTemplate {
	for i := range templates {
		if templates[i].Default {
			return &templates[i]
		}
	}
	if len(templates) > 0 {
		return &templates[0]
	}
	return nil
}

// DefaultTemplateByMode returns the default template filtered by mode.
func DefaultTemplateByMode(templates []PromptTemplate, mode string) *PromptTemplate {
	for i := range templates {
		if templates[i].Mode == mode && templates[i].Default {
			return &templates[i]
		}
	}
	for i := range templates {
		if templates[i].Mode == mode {
			return &templates[i]
		}
	}
	return DefaultTemplate(templates)
}

// LocalizeTemplates returns a deep copy of the template list with Name and
// Description replaced according to the given locale.  Fallback chain:
//
//	locale → primary language (e.g. "zh" from "zh-CN") → original Name/Description.
//
// The returned slice is safe to serialise directly; it never mutates the original.
func LocalizeTemplates(templates []PromptTemplate, locale string) []PromptTemplate {
	if len(templates) == 0 {
		return templates
	}
	out := make([]PromptTemplate, len(templates))
	copy(out, templates)
	for i := range out {
		if len(out[i].I18n) == 0 {
			continue
		}
		// Try exact match first (e.g. "zh-CN"), then primary subtag (e.g. "zh")
		l10n, ok := out[i].I18n[locale]
		if !ok {
			if idx := strings.IndexByte(locale, '-'); idx > 0 {
				l10n, ok = out[i].I18n[locale[:idx]]
			}
		}
		if !ok {
			continue
		}
		if l10n.Name != "" {
			out[i].Name = l10n.Name
		}
		if l10n.Description != "" {
			out[i].Description = l10n.Description
		}
	}
	return out
}

// ModelConfig 模型配置
type ModelConfig struct {
	Type       string                 `yaml:"type"       json:"type"`
	Source     string                 `yaml:"source"     json:"source"`
	ModelName  string                 `yaml:"model_name" json:"model_name"`
	Parameters map[string]interface{} `yaml:"parameters" json:"parameters"`
}

// StreamManagerConfig 流管理器配置
type StreamManagerConfig struct {
	Type           string        `yaml:"type"            json:"type"`            // 类型: "memory" 或 "redis"
	Redis          RedisConfig   `yaml:"redis"           json:"redis"`           // Redis配置
	CleanupTimeout time.Duration `yaml:"cleanup_timeout" json:"cleanup_timeout"` // 清理超时，单位秒
}

// RedisConfig Redis配置
type RedisConfig struct {
	Address  string        `yaml:"address"  json:"address"`  // Redis地址
	Username string        `yaml:"username" json:"username"` // Redis用户名
	Password string        `yaml:"password" json:"password"` // Redis密码
	DB       int           `yaml:"db"       json:"db"`       // Redis数据库
	Prefix   string        `yaml:"prefix"   json:"prefix"`   // 键前缀
	TTL      time.Duration `yaml:"ttl"      json:"ttl"`      // 过期时间(小时)
}

// ExtractManagerConfig 抽取管理器配置
type ExtractManagerConfig struct {
	ExtractGraph  *types.PromptTemplateStructured `yaml:"extract_graph"  json:"extract_graph"`
	ExtractEntity *types.PromptTemplateStructured `yaml:"extract_entity" json:"extract_entity"`
	FabriText     *FebriText                      `yaml:"fabri_text"     json:"fabri_text"`
}

type FebriText struct {
	WithTag   string `yaml:"with_tag"    json:"with_tag"`
	WithNoTag string `yaml:"with_no_tag" json:"with_no_tag"`
}

// resolvedConfigDir holds the directory of the loaded config file. Populated by
// LoadConfig and read by ConfigDir(); empty until LoadConfig has run.
var resolvedConfigDir string

// ConfigDir returns the directory containing the loaded config.yaml. Other
// startup code (e.g. builtin model loader) uses this to locate sibling config
// files like builtin_models.yaml without re-implementing viper search rules.
// Falls back to "./config" when LoadConfig has not been called yet.
func ConfigDir() string {
	if resolvedConfigDir != "" {
		return resolvedConfigDir
	}
	if f := viper.ConfigFileUsed(); f != "" {
		return filepath.Dir(f)
	}
	return "./config"
}

// LoadConfig 从配置文件加载配置
func LoadConfig() (*Config, error) {
	// 设置配置文件名和路径
	viper.SetConfigName("config")         // 配置文件名称(不带扩展名)
	viper.SetConfigType("yaml")           // 配置文件类型
	viper.AddConfigPath(".")              // 当前目录
	viper.AddConfigPath("./config")       // config子目录
	viper.AddConfigPath("$HOME/.appname") // 用户目录
	viper.AddConfigPath("/etc/appname/")  // etc目录

	// 启用环境变量替换
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	// 替换配置中的环境变量引用
	configFileContent, err := os.ReadFile(viper.ConfigFileUsed())
	if err != nil {
		return nil, fmt.Errorf("error reading config file content: %w", err)
	}

	// 替换${ENV_VAR}格式的环境变量引用
	re := regexp.MustCompile(`\${([^}]+)}`)
	result := re.ReplaceAllStringFunc(string(configFileContent), func(match string) string {
		// 提取环境变量名称（去掉${}部分）
		envVar := match[2 : len(match)-1]
		// 获取环境变量值，如果不存在则保持原样
		if value := os.Getenv(envVar); value != "" {
			return value
		}
		return match
	})

	// 使用处理后的配置内容
	viper.ReadConfig(strings.NewReader(result))

	// 解析配置到结构体
	var cfg Config
	if err := viper.Unmarshal(&cfg, func(dc *mapstructure.DecoderConfig) {
		dc.TagName = "yaml"
	}); err != nil {
		return nil, fmt.Errorf("unable to decode config into struct: %w", err)
	}
	fmt.Printf("Using configuration file: %s\n", viper.ConfigFileUsed())

	// 加载提示词模板（从目录或配置文件）
	configDir := filepath.Dir(viper.ConfigFileUsed())
	resolvedConfigDir = configDir
	promptTemplates, err := loadPromptTemplates(configDir)
	if err != nil {
		fmt.Printf("Warning: failed to load prompt templates from directory: %v\n", err)
		// 如果目录加载失败，使用配置文件中的模板（如果有）
	} else if promptTemplates != nil {
		cfg.PromptTemplates = promptTemplates
	}

	// Back-fill conversation config from prompt templates defaults
	// (so config.yaml can omit large prompt blocks and rely on template files)
	if cfg.PromptTemplates != nil && cfg.Conversation != nil {
		backfillConversationDefaults(&cfg)
	}

	// Load built-in agent definitions (i18n-aware) from builtin_agents.yaml
	if err := types.LoadBuiltinAgentsConfig(configDir); err != nil {
		fmt.Printf("Warning: failed to load builtin agents config: %v\n", err)
	}

	// Load smart-reasoning agent type presets (rag-qa / wiki-qa / hybrid / custom).
	if err := types.LoadAgentTypePresetsConfig(configDir); err != nil {
		fmt.Printf("Warning: failed to load agent type presets: %v\n", err)
	}

	// Resolve prompt template ID references in builtin agent configs
	// (e.g. system_prompt_id -> actual content from agent_system_prompt.yaml)
	if cfg.PromptTemplates != nil {
		resolveBuiltinAgentPromptIDs(cfg.PromptTemplates)
		// Validate that every preset references an existing prompt template.
		types.ResolveAgentTypePresetPromptRefs(func(id string) string {
			if t := FindTemplateByID(cfg.PromptTemplates, id); t != nil {
				return t.Content
			}
			return ""
		})
	}

	// Validate configuration values
	applyOIDCEnvOverrides(&cfg)
	applyAgentEnvOverrides(&cfg)
	applyKnowledgeBaseEnvOverrides(&cfg)
	applyAuthAndTenantDefaults(&cfg)
	applyAuditDefaults(&cfg)
	applyBackupEnvOverrides(&cfg)

	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}

	// Surface RBAC enforcement state at startup. air's hot-reload only
	// rebuilds the binary on Go-source changes; it does NOT re-source
	// .env, so a `WEKNORA_TENANT_ENABLE_RBAC=true` flip while the dev
	// loop is already running silently has no effect until the dev
	// script restarts. Logging this once at startup makes the
	// "I edited .env but the gates still aren't firing" trap obvious
	// from the first console line. Printf rather than logger because
	// LoadConfig runs before the logger sink is wired in the dig graph.
	rbacOn := cfg.Tenant.IsRBACEnforced()
	xtAccess := cfg.Tenant != nil && cfg.Tenant.EnableCrossTenantAccess
	fmt.Printf(
		"[config] tenant RBAC enforcement: enable_rbac=%v cross_tenant_access=%v "+
			"(env: WEKNORA_TENANT_ENABLE_RBAC=%q WEKNORA_TENANT_ENABLE_CROSS_TENANT_ACCESS=%q)\n",
		rbacOn, xtAccess,
		os.Getenv("WEKNORA_TENANT_ENABLE_RBAC"),
		os.Getenv("WEKNORA_TENANT_ENABLE_CROSS_TENANT_ACCESS"),
	)

	return &cfg, nil
}

// ValidateConfig performs basic validation of the loaded configuration.
// It checks for obviously invalid or missing values that would cause runtime failures.
func ValidateConfig(cfg *Config) error {
	var errs []string

	if cfg.OIDCAuth != nil && cfg.OIDCAuth.Enable {
		if strings.TrimSpace(cfg.OIDCAuth.ClientID) == "" {
			errs = append(errs, "oidc_auth.client_id is required when OIDC is enabled")
		}
		if strings.TrimSpace(cfg.OIDCAuth.ClientSecret) == "" {
			errs = append(errs, "oidc_auth.client_secret is required when OIDC is enabled")
		}
		if strings.TrimSpace(cfg.OIDCAuth.DiscoveryURL) == "" &&
			(strings.TrimSpace(cfg.OIDCAuth.AuthorizationEndpoint) == "" || strings.TrimSpace(cfg.OIDCAuth.TokenEndpoint) == "") {
			errs = append(errs, "oidc_auth.discovery_url or both oidc_auth.authorization_endpoint and oidc_auth.token_endpoint are required when OIDC is enabled")
		}
	}

	if cfg.Auth != nil {
		mode := strings.TrimSpace(cfg.Auth.RegistrationMode)
		if mode != "" && mode != AuthRegistrationModeSelfServe && mode != AuthRegistrationModeInviteOnly {
			errs = append(errs, fmt.Sprintf("auth.registration_mode must be %q or %q, got %q",
				AuthRegistrationModeSelfServe, AuthRegistrationModeInviteOnly, mode))
		}
		tenantMode := strings.TrimSpace(cfg.Auth.DefaultTenantMode)
		if tenantMode != "" && tenantMode != AuthDefaultTenantModeCreatePersonal && tenantMode != AuthDefaultTenantModeTenantless {
			errs = append(errs, fmt.Sprintf("auth.default_tenant_mode must be %q or %q, got %q",
				AuthDefaultTenantModeCreatePersonal, AuthDefaultTenantModeTenantless, tenantMode))
		}
	}

	if cfg.Audit != nil && cfg.Audit.RetentionDays < 0 {
		errs = append(errs, fmt.Sprintf("audit.retention_days must be >= 0 (got %d); use 0 to disable purge",
			cfg.Audit.RetentionDays))
	}

	// Backup section (PRD data-backup-recovery §7): a disabled section
	// is fully inert; an enabled one must carry a usable target.
	if cfg.Backup != nil && cfg.Backup.Enabled {
		if err := validateBackupConfig(cfg.Backup); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if cfg.Conversation != nil {
		if cfg.Conversation.EmbeddingTopK < 0 {
			errs = append(errs, "conversation.embedding_top_k must be >= 0")
		}
		if cfg.Conversation.RerankTopK < 0 {
			errs = append(errs, "conversation.rerank_top_k must be >= 0")
		}
		if cfg.Conversation.VectorThreshold < 0 || cfg.Conversation.VectorThreshold > 1 {
			errs = append(errs, "conversation.vector_threshold must be between 0 and 1")
		}
		if cfg.Conversation.RerankThreshold < -10 || cfg.Conversation.RerankThreshold > 10 {
			errs = append(errs, "conversation.rerank_threshold must be between -10 and 10")
		}
	}

	if cfg.KnowledgeBase != nil {
		if cfg.KnowledgeBase.ChunkSize <= 0 {
			errs = append(errs, "knowledge_base.chunk_size must be > 0")
		}
		if cfg.KnowledgeBase.ChunkOverlap < 0 {
			errs = append(errs, "knowledge_base.chunk_overlap must be >= 0")
		}
		if cfg.KnowledgeBase.ChunkOverlap >= cfg.KnowledgeBase.ChunkSize {
			errs = append(errs, "knowledge_base.chunk_overlap must be less than chunk_size")
		}
	}

	if cfg.Server != nil {
		if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
			errs = append(errs, "server.port must be between 1 and 65535")
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func applyOIDCEnvOverrides(cfg *Config) {
	if cfg.OIDCAuth == nil {
		cfg.OIDCAuth = &OIDCAuthConfig{}
	}
	if cfg.OIDCAuth.UserInfoMapping == nil {
		cfg.OIDCAuth.UserInfoMapping = &OIDCUserInfoMapping{}
	}

	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_ENABLE")); value != "" {
		cfg.OIDCAuth.Enable = strings.EqualFold(value, "true")
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_ISSUER_URL")); value != "" {
		cfg.OIDCAuth.IssuerURL = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_DISCOVERY_URL")); value != "" {
		cfg.OIDCAuth.DiscoveryURL = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_PROVIDER_DISPLAY_NAME")); value != "" {
		cfg.OIDCAuth.ProviderDisplayName = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_CLIENT_ID")); value != "" {
		cfg.OIDCAuth.ClientID = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_CLIENT_SECRET")); value != "" {
		cfg.OIDCAuth.ClientSecret = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_AUTHORIZATION_ENDPOINT")); value != "" {
		cfg.OIDCAuth.AuthorizationEndpoint = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_TOKEN_ENDPOINT")); value != "" {
		cfg.OIDCAuth.TokenEndpoint = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_USER_INFO_ENDPOINT")); value != "" {
		cfg.OIDCAuth.UserInfoEndpoint = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_AUTH_SCOPES")); value != "" {
		cfg.OIDCAuth.Scopes = strings.Fields(strings.ReplaceAll(value, ",", " "))
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_USER_INFO_MAPPING_USER_NAME")); value != "" {
		cfg.OIDCAuth.UserInfoMapping.Username = value
	}
	if value := strings.TrimSpace(os.Getenv("OIDC_USER_INFO_MAPPING_EMAIL")); value != "" {
		cfg.OIDCAuth.UserInfoMapping.Email = value
	}

	if cfg.OIDCAuth.ProviderDisplayName == "" {
		cfg.OIDCAuth.ProviderDisplayName = "OIDC"
	}
	if len(cfg.OIDCAuth.Scopes) == 0 {
		cfg.OIDCAuth.Scopes = []string{"openid", "profile", "email"}
	}
	if cfg.OIDCAuth.UserInfoMapping.Username == "" {
		cfg.OIDCAuth.UserInfoMapping.Username = "name"
	}
	if cfg.OIDCAuth.UserInfoMapping.Email == "" {
		cfg.OIDCAuth.UserInfoMapping.Email = "email"
	}
	if cfg.OIDCAuth.DiscoveryURL == "" && cfg.OIDCAuth.IssuerURL != "" {
		cfg.OIDCAuth.DiscoveryURL = strings.TrimRight(cfg.OIDCAuth.IssuerURL, "/") + "/.well-known/openid-configuration"
	}
}

func applyKnowledgeBaseEnvOverrides(cfg *Config) {
	if cfg.KnowledgeBase == nil {
		cfg.KnowledgeBase = &KnowledgeBaseConfig{}
	}
	if cfg.KnowledgeBase.DocumentProcessTimeout <= 0 {
		cfg.KnowledgeBase.DocumentProcessTimeout = DefaultDocumentProcessTimeout
	}
	if value := strings.TrimSpace(os.Getenv("WEKNORA_DOCUMENT_PROCESS_TIMEOUT")); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			cfg.KnowledgeBase.DocumentProcessTimeout = d
		}
	}
	if cfg.KnowledgeBase.DocReaderCallTimeout <= 0 {
		cfg.KnowledgeBase.DocReaderCallTimeout = 30 * time.Minute
	}
	if value := strings.TrimSpace(os.Getenv("WEKNORA_DOCREADER_CALL_TIMEOUT")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			cfg.KnowledgeBase.DocReaderCallTimeout = d
		}
	}
}

func applyAgentEnvOverrides(cfg *Config) {
	if cfg.Agent == nil {
		cfg.Agent = &AgentConfig{}
	}
	if value := strings.TrimSpace(os.Getenv("WEKNORA_AGENT_LLM_TIMEOUT")); value != "" {
		if timeout, err := time.ParseDuration(value); err == nil {
			cfg.Agent.LLMCallTimeout = int(timeout.Seconds())
		} else if sec, err := time.ParseDuration(value + "s"); err == nil {
			// Handle case where user just provides a number like "300"
			cfg.Agent.LLMCallTimeout = int(sec.Seconds())
		}
	}
	// MCP tool human-approval wait timeout (issue #1173). Accepts Go duration
	// (e.g. "10m", "30s") or a bare number interpreted as seconds.
	if value := strings.TrimSpace(os.Getenv("WEKNORA_AGENT_TOOL_APPROVAL_TIMEOUT")); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			cfg.Agent.ToolApprovalTimeoutSeconds = int(d.Seconds())
		} else if d, err := time.ParseDuration(value + "s"); err == nil {
			cfg.Agent.ToolApprovalTimeoutSeconds = int(d.Seconds())
		}
	}
}

// applyAuthAndTenantDefaults fills in defaults for the Auth and Tenant
// config sections and applies env-var overrides that operators commonly use
// to enable RBAC or switch registration mode without editing config.yaml.
//
// Defaults:
//   - auth.registration_mode  -> "self_serve" (preserves pre-RBAC behaviour)
//   - auth.default_tenant_mode -> "create_personal" (preserves the
//     historical registration behaviour)
//   - tenant.enable_rbac      -> true (enforce role checks unless an
//     operator explicitly opts into the logging-only rollout window via
//     config.yaml `enable_rbac: false` or `WEKNORA_TENANT_ENABLE_RBAC=false`).
//   - tenant.self_service_creation_enabled -> true (preserves ordinary
//     authenticated users' ability to create workspaces).
//
// Env overrides (when set and non-empty):
//   - WEKNORA_AUTH_DEFAULT_TENANT_MODE ("create_personal"/"tenantless")
//   - WEKNORA_TENANT_SELF_SERVICE_CREATION_ENABLED (boolean)
//   - WEKNORA_TENANT_ENABLE_RBAC      ("true"/"false", case-insensitive)
//   - WEKNORA_TENANT_MAX_OWNED_PER_USER (integer; <0 disables the cap,
//     0 falls back to the handler default, >0 enforces that exact cap).
//     Unparseable / empty values are ignored so a stale shell variable
//     can't silently disable the quota for a future deployment.
//
// Note: auth.registration_mode has no dedicated env override. The
// long-standing DISABLE_REGISTRATION=true env var is the single env-layer
// knob and, when set, coerces registration_mode to invite_only here. That
// way both the API gate (handler) and the /auth/config-driven UI gate
// (frontend hides the register entry) stay consistent — without needing
// two parallel env vars.
func applyAuthAndTenantDefaults(cfg *Config) {
	if cfg.Auth == nil {
		cfg.Auth = &AuthConfig{}
	}
	if cfg.Tenant == nil {
		cfg.Tenant = &TenantConfig{}
	}

	if legacy := strings.TrimSpace(os.Getenv("DISABLE_REGISTRATION")); strings.EqualFold(legacy, "true") {
		prev := strings.TrimSpace(cfg.Auth.RegistrationMode)
		cfg.Auth.RegistrationMode = AuthRegistrationModeInviteOnly
		if prev != "" && prev != AuthRegistrationModeInviteOnly {
			fmt.Printf(
				"[config] DISABLE_REGISTRATION=true overrides auth.registration_mode=%q -> %q\n",
				prev, AuthRegistrationModeInviteOnly,
			)
		}
	}

	if strings.TrimSpace(cfg.Auth.RegistrationMode) == "" {
		cfg.Auth.RegistrationMode = AuthRegistrationModeSelfServe
	}
	if value := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_DEFAULT_TENANT_MODE")); value != "" {
		cfg.Auth.DefaultTenantMode = value
	}
	if strings.TrimSpace(cfg.Auth.DefaultTenantMode) == "" {
		cfg.Auth.DefaultTenantMode = AuthDefaultTenantModeCreatePersonal
	}
	applyAuthVerificationDefaults(cfg.Auth)


	if value := strings.TrimSpace(os.Getenv("WEKNORA_TENANT_ENABLE_RBAC")); value != "" {
		v := strings.EqualFold(value, "true")
		cfg.Tenant.EnableRBAC = &v
	}
	if cfg.Tenant.EnableRBAC == nil {
		// Default: enforce. Operators opt out of enforcement explicitly
		// via config.yaml `enable_rbac: false` or the env override.
		on := true
		cfg.Tenant.EnableRBAC = &on
	}

	if value := strings.TrimSpace(os.Getenv("WEKNORA_TENANT_SELF_SERVICE_CREATION_ENABLED")); value != "" {
		if enabled, err := strconv.ParseBool(value); err == nil {
			cfg.Tenant.SelfServiceCreationEnabled = &enabled
		} else {
			fmt.Printf(
				"[config] WEKNORA_TENANT_SELF_SERVICE_CREATION_ENABLED=%q is not a boolean, ignoring\n",
				value,
			)
		}
	}
	if cfg.Tenant.SelfServiceCreationEnabled == nil {
		on := true
		cfg.Tenant.SelfServiceCreationEnabled = &on
	}

	if value := strings.TrimSpace(os.Getenv("WEKNORA_TENANT_MAX_OWNED_PER_USER")); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			cfg.Tenant.MaxOwnedPerUser = n
		} else {
			fmt.Printf(
				"[config] WEKNORA_TENANT_MAX_OWNED_PER_USER=%q is not an integer, ignoring\n",
				value,
			)
		}
	}
}

// applyAuthVerificationDefaults fills in defaults and env overrides for the
// P0-4 auth verification sections (captcha / verification_code / sms / email).
//
// Defaults:
//   - auth.captcha.type          -> "slider"
//   - auth.captcha.login_required-> true
//   - auth.verification_code.*   -> 6 digits / 10 min TTL / 60s resend
//                                    interval / 10 per target per day /
//                                    5 failed attempts
//   - auth.sms.provider          -> "log" (zero-config safe: codes go to
//                                    the server log; aliyun requires
//                                    explicit credentials)
//   - auth.email.provider        -> "log" (same rationale)
//
// Env overrides (when set and non-empty):
//   - WEKNORA_AUTH_CAPTCHA_TYPE / WEKNORA_AUTH_CAPTCHA_LOGIN_REQUIRED
//   - WEKNORA_AUTH_SMS_PROVIDER, WEKNORA_AUTH_SMS_ALIYUN_ACCESS_KEY_ID,
//     WEKNORA_AUTH_SMS_ALIYUN_ACCESS_KEY_SECRET,
//     WEKNORA_AUTH_SMS_ALIYUN_SIGN_NAME, WEKNORA_AUTH_SMS_ALIYUN_TEMPLATE_CODE
//   - WEKNORA_AUTH_EMAIL_PROVIDER, WEKNORA_AUTH_EMAIL_SMTP_HOST/PORT/
//     USERNAME/PASSWORD/FROM/FROM_ADDR, and WEKNORA_AUTH_EMAIL_SMTP_PRESET
//     (qq/163/126/gmail/exmail/aliyun/outlook — fills host/port/use_ssl
//     with the provider's standard values when not set explicitly)
//
// Channel availability semantics (PRD §6.3): a channel is usable when its
// provider is fully configured ("aliyun"/"smtp" with credentials, or the
// explicit "log" provider). With both channels on "log" and no SMTP/SMS
// credentials the register form falls back to the classic email+password
// flow, so zero-config deployments keep working after upgrade.
func applyAuthVerificationDefaults(auth *AuthConfig) {
	if auth.Captcha == nil {
		auth.Captcha = &AuthCaptchaConfig{}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_CAPTCHA_TYPE")); v != "" {
		auth.Captcha.Type = v
	}
	if strings.TrimSpace(auth.Captcha.Type) == "" {
		auth.Captcha.Type = "slider"
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_CAPTCHA_LOGIN_REQUIRED")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			auth.Captcha.LoginRequired = &b
		}
	}
	if auth.Captcha.LoginRequired == nil {
		on := true
		auth.Captcha.LoginRequired = &on
	}

	vc := auth.VerificationCode
	if vc == nil {
		vc = &AuthVerificationCodeConfig{}
		auth.VerificationCode = vc
	}
	if vc.Length == 0 {
		vc.Length = 6
	}
	if vc.TTLMinutes == 0 {
		vc.TTLMinutes = 10
	}
	if vc.ResendIntervalSeconds == 0 {
		vc.ResendIntervalSeconds = 60
	}
	if vc.DailyLimitPerTarget == 0 {
		vc.DailyLimitPerTarget = 10
	}
	if vc.MaxAttempts == 0 {
		vc.MaxAttempts = 5
	}

	if auth.SMS == nil {
		auth.SMS = &AuthSMSConfig{}
	}
	if auth.SMS.Aliyun == nil {
		auth.SMS.Aliyun = &AuthAliyunSMSConfig{}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_SMS_PROVIDER")); v != "" {
		auth.SMS.Provider = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_SMS_ALIYUN_ACCESS_KEY_ID")); v != "" {
		auth.SMS.Aliyun.AccessKeyID = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_SMS_ALIYUN_ACCESS_KEY_SECRET")); v != "" {
		auth.SMS.Aliyun.AccessKeySecret = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_SMS_ALIYUN_SIGN_NAME")); v != "" {
		auth.SMS.Aliyun.SignName = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_SMS_ALIYUN_TEMPLATE_CODE")); v != "" {
		auth.SMS.Aliyun.TemplateCode = v
	}
	// Zero-config default: "log". An explicit aliyun provider only counts as
	// usable when its credentials are complete (validated in the service).
	if strings.TrimSpace(auth.SMS.Provider) == "" {
		auth.SMS.Provider = "log"
	}

	if auth.Email == nil {
		auth.Email = &AuthEmailCodeConfig{}
	}
	if auth.Email.SMTP == nil {
		auth.Email.SMTP = &AuthSMTPConfig{}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_PROVIDER")); v != "" {
		auth.Email.Provider = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_PRESET")); v != "" {
		auth.Email.SMTP.Preset = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_HOST")); v != "" {
		auth.Email.SMTP.Host = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			auth.Email.SMTP.Port = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_USERNAME")); v != "" {
		auth.Email.SMTP.Username = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_PASSWORD")); v != "" {
		auth.Email.SMTP.Password = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_AUTH_EMAIL_SMTP_FROM")); v != "" {
		auth.Email.SMTP.From = v
	}
	if strings.TrimSpace(auth.Email.Provider) == "" {
		auth.Email.Provider = "log"
	}
	// Preset resolution comes last and only fills empty fields: an explicit
	// host/port/use_ssl from yaml or env always wins over the preset.
	applySMTPPresetDefaults(auth.Email.SMTP)
}

// SMSEnabled reports whether the SMS channel can deliver codes: either the
// log provider (dev) or a fully-credentialed aliyun provider.
func (c *AuthConfig) SMSEnabled() bool {
	if c == nil || c.SMS == nil {
		return false
	}
	switch strings.TrimSpace(c.SMS.Provider) {
	case "log":
		return true
	case "aliyun":
		return c.SMS.Aliyun != nil &&
			c.SMS.Aliyun.AccessKeyID != "" &&
			c.SMS.Aliyun.AccessKeySecret != "" &&
			c.SMS.Aliyun.SignName != "" &&
			c.SMS.Aliyun.TemplateCode != ""
	default:
		return false
	}
}

// EmailCodeEnabled reports whether the email channel can deliver codes:
// either the log provider (dev) or an smtp provider with a host configured.
func (c *AuthConfig) EmailCodeEnabled() bool {
	if c == nil || c.Email == nil {
		return false
	}
	switch strings.TrimSpace(c.Email.Provider) {
	case "log":
		return true
	case "smtp":
		return c.Email.SMTP != nil && strings.TrimSpace(c.Email.SMTP.Host) != ""
	default:
		return false
	}
}

// CaptchaLoginRequired reports whether POST /auth/login must present a
// captcha token. Default true.
func (c *AuthConfig) CaptchaLoginRequired() bool {
	if c == nil || c.Captcha == nil || c.Captcha.LoginRequired == nil {
		return true
	}
	return *c.Captcha.LoginRequired
}

// applyAuditDefaults fills in defaults for the Audit config section
// and applies the env override commonly used to extend or disable
// retention without editing config.yaml.
//
// Defaults:
//   - When the `audit:` section is omitted entirely from YAML,
//     RetentionDays = 90 (purge rows older than 90 days).
//
// Operator intent is otherwise preserved: an explicit
// `audit.retention_days: 0` in YAML means "disable the purge", which
// is a supported posture for compliance use cases that handle archival
// off-database.
//
// Env overrides (when set and parseable; out-of-range is ignored):
//   - WEKNORA_AUDIT_RETENTION_DAYS (non-negative integer)
func applyAuditDefaults(cfg *Config) {
	// Section omitted entirely -> apply the default and no env wiring
	// is needed for the most common path.
	if cfg.Audit == nil {
		cfg.Audit = &AuditConfig{RetentionDays: 90}
	}

	// Env override always wins, but only when explicitly set so a
	// stale shell variable doesn't suddenly disable the purge for a
	// future deployment that committed a real value.
	if value := strings.TrimSpace(os.Getenv("WEKNORA_AUDIT_RETENTION_DAYS")); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n >= 0 {
			cfg.Audit.RetentionDays = n
		}
	}
}

// applyBackupEnvOverrides fills backup defaults and applies
// WEKNORA_BACKUP_* env overrides (PRD docs/prd/data-backup-recovery.md §7).
// Env always wins when explicitly set; defaults keep the section inert
// (enabled=false) so upgrading deployments see no behaviour change.
func applyBackupEnvOverrides(cfg *Config) {
	if cfg.Backup == nil {
		cfg.Backup = &BackupConfig{}
	}
	b := cfg.Backup

	if b.Schedule == "" {
		b.Schedule = "0 0 3 * * *"
	}
	if b.RetentionDaily == 0 {
		b.RetentionDaily = 7
	}
	if b.RetentionWeekly == 0 {
		b.RetentionWeekly = 4
	}
	if b.RetentionMonthly == 0 {
		b.RetentionMonthly = 6
	}
	if b.Compression == "" {
		b.Compression = "gzip"
	}
	if b.ConcurrencyTenants == 0 {
		b.ConcurrencyTenants = 2
	}
	if b.ConcurrencyObjects == 0 {
		b.ConcurrencyObjects = 8
	}
	if b.Storage == nil {
		b.Storage = &BackupStorageConfig{}
	}
	if b.Storage.PathPrefix == "" {
		b.Storage.PathPrefix = "backups"
	}

	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_ENABLED")); v != "" {
		b.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_SCHEDULE")); v != "" {
		b.Schedule = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_RETENTION_DAILY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			b.RetentionDaily = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_RETENTION_WEEKLY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			b.RetentionWeekly = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_RETENTION_MONTHLY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			b.RetentionMonthly = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_COMPRESSION")); v != "" {
		b.Compression = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_ENCRYPT")); v != "" {
		b.Encrypt = strings.EqualFold(v, "true") || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_CONCURRENCY_TENANTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.ConcurrencyTenants = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_CONCURRENCY_OBJECTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.ConcurrencyObjects = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_PRE_DELETE_SNAPSHOT")); v != "" {
		enabled := strings.EqualFold(v, "true") || v == "1"
		b.PreDeleteSnapshot = &enabled
	}

	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_PROVIDER")); v != "" {
		b.Storage.Provider = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_LOCAL_PATH")); v != "" {
		b.Storage.LocalPath = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_ENDPOINT")); v != "" {
		b.Storage.Endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_ACCESS_KEY")); v != "" {
		b.Storage.AccessKey = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_SECRET_KEY")); v != "" {
		b.Storage.SecretKey = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_BUCKET")); v != "" {
		b.Storage.Bucket = v
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_USE_SSL")); v != "" {
		useSSL := strings.EqualFold(v, "true") || v == "1"
		b.Storage.UseSSL = &useSSL
	}
	if v := strings.TrimSpace(os.Getenv("WEKNORA_BACKUP_STORAGE_PATH_PREFIX")); v != "" {
		b.Storage.PathPrefix = v
	}
}

// validateBackupConfig gates an ENABLED backup section: the schedule must
// parse, retention tiers must be non-negative, compression must be a known
// flavour, and the storage target must be complete enough to construct.
// The guard against "backup target == primary storage" happens at container
// wiring time (where the primary storage config is visible), not here.
func validateBackupConfig(b *BackupConfig) error {
	if b.Schedule != "" {
		if _, err := cron.ParseStandard(b.Schedule); err != nil {
			// Try the 6-field (seconds-first) form used by the datasource
			// scheduler before rejecting.
			if _, err6 := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(b.Schedule); err6 != nil {
				return fmt.Errorf("backup.schedule is not a valid cron expression: %q (%v)", b.Schedule, err)
			}
		}
	}
	if b.RetentionDaily < 0 || b.RetentionWeekly < 0 || b.RetentionMonthly < 0 {
		return fmt.Errorf("backup retention tiers must be >= 0 (got daily=%d weekly=%d monthly=%d)",
			b.RetentionDaily, b.RetentionWeekly, b.RetentionMonthly)
	}
	if b.Compression != "" && b.Compression != "gzip" && b.Compression != "none" {
		return fmt.Errorf("backup.compression must be gzip or none (got %q)", b.Compression)
	}
	if b.ConcurrencyTenants < 0 || b.ConcurrencyObjects < 0 {
		return fmt.Errorf("backup concurrency values must be >= 0")
	}

	if b.Storage == nil {
		return fmt.Errorf("backup.storage is required when backup.enabled is true")
	}
	switch b.Storage.Provider {
	case "":
		return fmt.Errorf("backup.storage.provider is required when backup.enabled is true (local | minio | s3)")
	case "local":
		if strings.TrimSpace(b.Storage.LocalPath) == "" {
			return fmt.Errorf("backup.storage.local_path is required for provider=local")
		}
	case "minio", "s3":
		if strings.TrimSpace(b.Storage.Endpoint) == "" || strings.TrimSpace(b.Storage.AccessKey) == "" ||
			strings.TrimSpace(b.Storage.SecretKey) == "" || strings.TrimSpace(b.Storage.Bucket) == "" {
			return fmt.Errorf("backup.storage endpoint/access_key/secret_key/bucket are all required for provider=%s", b.Storage.Provider)
		}
	default:
		return fmt.Errorf("backup.storage.provider %q is not supported for the backup target; use local, minio, or s3 (S3-compatible endpoint)", b.Storage.Provider)
	}
	return nil
}

// into actual prompt text content. Only xxx_id fields are used;
// no fallback to default templates.
func backfillConversationDefaults(cfg *Config) {
	pt := cfg.PromptTemplates
	conv := cfg.Conversation

	if conv.FallbackPromptID != "" {
		if t := FindTemplateByID(pt, conv.FallbackPromptID); t != nil {
			conv.FallbackPrompt = t.Content
		} else {
			fmt.Printf("Warning: fallback_prompt_id %q not found\n", conv.FallbackPromptID)
		}
	}
	if conv.RewritePromptID != "" {
		if t := FindTemplateByID(pt, conv.RewritePromptID); t != nil {
			conv.RewritePromptSystem = t.Content
			conv.RewritePromptUser = t.User
		} else {
			fmt.Printf("Warning: rewrite_prompt_id %q not found\n", conv.RewritePromptID)
		}
	}
	if conv.GenerateSessionTitlePromptID != "" {
		if t := FindTemplateByID(pt, conv.GenerateSessionTitlePromptID); t != nil {
			conv.GenerateSessionTitlePrompt = t.Content
		} else {
			fmt.Printf("Warning: generate_session_title_prompt_id %q not found\n", conv.GenerateSessionTitlePromptID)
		}
	}
	if conv.GenerateSummaryPromptID != "" {
		if t := FindTemplateByID(pt, conv.GenerateSummaryPromptID); t != nil {
			conv.GenerateSummaryPrompt = t.Content
		} else {
			fmt.Printf("Warning: generate_summary_prompt_id %q not found\n", conv.GenerateSummaryPromptID)
		}
	}
	if conv.ExtractEntitiesPromptID != "" {
		if t := FindTemplateByID(pt, conv.ExtractEntitiesPromptID); t != nil {
			conv.ExtractEntitiesPrompt = t.Content
		} else {
			fmt.Printf("Warning: extract_entities_prompt_id %q not found\n", conv.ExtractEntitiesPromptID)
		}
	}
	if conv.ExtractRelationshipsPromptID != "" {
		if t := FindTemplateByID(pt, conv.ExtractRelationshipsPromptID); t != nil {
			conv.ExtractRelationshipsPrompt = t.Content
		} else {
			fmt.Printf("Warning: extract_relationships_prompt_id %q not found\n", conv.ExtractRelationshipsPromptID)
		}
	}
	if conv.GenerateQuestionsPromptID != "" {
		if t := FindTemplateByID(pt, conv.GenerateQuestionsPromptID); t != nil {
			conv.GenerateQuestionsPrompt = t.Content
		} else {
			fmt.Printf("Warning: generate_questions_prompt_id %q not found\n", conv.GenerateQuestionsPromptID)
		}
	}
	if conv.MemoryExtractionPromptID != "" {
		if t := FindTemplateByID(pt, conv.MemoryExtractionPromptID); t != nil {
			conv.MemoryExtractionPrompt = t.Content
		} else {
			fmt.Printf("Warning: memory_extraction_prompt_id %q not found\n", conv.MemoryExtractionPromptID)
		}
	}
	if conv.SessionPrecipitationPromptID != "" {
		if t := FindTemplateByID(pt, conv.SessionPrecipitationPromptID); t != nil {
			conv.SessionPrecipitationPrompt = t.Content
		} else {
			fmt.Printf("Warning: session_precipitation_prompt_id %q not found\n", conv.SessionPrecipitationPromptID)
		}
	}
	if conv.SessionWikiPromptID != "" {
		if t := FindTemplateByID(pt, conv.SessionWikiPromptID); t != nil {
			conv.SessionWikiPrompt = t.Content
		} else {
			fmt.Printf("Warning: session_wiki_prompt_id %q not found\n", conv.SessionWikiPromptID)
		}
	}
	if conv.Summary != nil {
		if conv.Summary.PromptID != "" {
			if t := FindTemplateByID(pt, conv.Summary.PromptID); t != nil {
				conv.Summary.Prompt = t.Content
			} else {
				fmt.Printf("Warning: summary.prompt_id %q not found\n", conv.Summary.PromptID)
			}
		}
		if conv.Summary.ContextTemplateID != "" {
			if t := FindTemplateByID(pt, conv.Summary.ContextTemplateID); t != nil {
				conv.Summary.ContextTemplate = t.Content
			} else {
				fmt.Printf("Warning: summary.context_template_id %q not found\n", conv.Summary.ContextTemplateID)
			}
		}
	}

	// Build intent→system-prompt map from IntentPrompts templates.
	// Template ID must equal the QueryIntent string value (e.g. "greeting").
	if len(pt.IntentPrompts) > 0 {
		conv.IntentSystemPrompts = make(map[string]string, len(pt.IntentPrompts))
		for _, t := range pt.IntentPrompts {
			if t.ID != "" && t.Content != "" {
				conv.IntentSystemPrompts[t.ID] = t.Content
			}
		}
	}
}

// FindTemplateByID searches across all template lists for a template with the given ID.
// It returns the template if found, or nil otherwise.
func FindTemplateByID(pt *PromptTemplatesConfig, id string) *PromptTemplate {
	if pt == nil || id == "" {
		return nil
	}
	// Search all template collections
	for _, list := range [][]PromptTemplate{
		pt.SystemPrompt,
		pt.ContextTemplate,
		pt.Rewrite,
		pt.Fallback,
		pt.GenerateSessionTitle,
		pt.GenerateSummary,
		pt.KeywordsExtraction,
		pt.AgentSystemPrompt,
		pt.GraphExtraction,
		pt.GenerateQuestions,
		pt.IntentPrompts,
	} {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
	}
	return nil
}

// resolveBuiltinAgentPromptIDs resolves system_prompt_id and context_template_id
// references in builtin agent configs by looking up the actual content from
// prompt template YAML files.
func resolveBuiltinAgentPromptIDs(pt *PromptTemplatesConfig) {
	types.ResolveBuiltinAgentPromptRefs(func(id string) string {
		if t := FindTemplateByID(pt, id); t != nil {
			return t.Content
		}
		return ""
	})
}

// promptTemplateFile 用于解析模板文件
type promptTemplateFile struct {
	Templates []PromptTemplate `yaml:"templates"`
}

// loadPromptTemplates 从目录加载提示词模板
func loadPromptTemplates(configDir string) (*PromptTemplatesConfig, error) {
	templatesDir := filepath.Join(configDir, "prompt_templates")

	// 检查目录是否存在
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		return nil, nil // 目录不存在，返回nil让调用者使用配置文件中的模板
	}

	config := &PromptTemplatesConfig{}

	// 定义模板文件映射
	templateFiles := map[string]*[]PromptTemplate{
		"system_prompt.yaml":          &config.SystemPrompt,
		"context_template.yaml":       &config.ContextTemplate,
		"rewrite.yaml":                &config.Rewrite,
		"fallback.yaml":               &config.Fallback,
		"generate_session_title.yaml": &config.GenerateSessionTitle,
		"generate_summary.yaml":       &config.GenerateSummary,
		"keywords_extraction.yaml":    &config.KeywordsExtraction,
		"agent_system_prompt.yaml":    &config.AgentSystemPrompt,
		"graph_extraction.yaml":       &config.GraphExtraction,
		"memory_extraction.yaml":      &config.MemoryExtraction,
		"session_precipitation.yaml":  &config.SessionPrecipitation,
		"generate_questions.yaml":     &config.GenerateQuestions,
		"intent_prompts.yaml":         &config.IntentPrompts,
	}

	// 加载每个模板文件
	for filename, target := range templateFiles {
		filePath := filepath.Join(templatesDir, filename)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			continue // 文件不存在，跳过
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", filename, err)
		}

		var file promptTemplateFile
		if err := yaml.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", filename, err)
		}

		*target = file.Templates
	}

	return config, nil
}

// WebSearchConfig represents the web search configuration
type WebSearchConfig struct {
	Timeout int `yaml:"timeout" json:"timeout"` // 超时时间（秒）
}

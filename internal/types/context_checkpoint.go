package types

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ContextCheckpoint 是一份 Agent 上下文压缩摘要，持久化在它所覆盖的最后一轮的
// assistant 消息行上（PRD FR-10）。下一轮装载历史时从最新的检查点起读：摘要替代
// 该轮及其之前的全部原始轮次，于是一次压缩只付一次摘要模型调用，而不是每轮重付。
//
// 把检查点存在「被覆盖的那一轮」而不是「执行压缩的那一轮」上，使边界成为隐式身份：
// 复制该轮就复制了检查点，删除该轮也就删除了检查点。
//
// 与上游（v0.9.0）不同，Zilan 额外要求前缀严格一致（PRD FR-10 规则 3）：PrefixHash
// 覆盖被压缩区间内每条持久化消息的内容，任何编辑/撤回/重新生成都会让重算结果不匹配，
// 检查点随即失效并退回全量重算。
type ContextCheckpoint struct {
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
	// Degraded 标记摘要模型失败后留下的规则式归档。下一次压缩会把它当作上一份摘要
	// 折进去，因此它是被精炼而不是被原样继承。
	Degraded bool `json:"degraded,omitempty"`
	// PrefixHash 是被覆盖区间 [AnchorTurnID, 本行所在轮次] 的链式内容哈希，
	// 复用前必须重算并逐字节相等（见 FoldStoredMessages）。
	PrefixHash string `json:"prefix_hash,omitempty"`
	// AnchorTurnID 是被覆盖区间的起点轮次（types.Message.RequestID，含）。
	// 空表示从会话最早的一轮开始。
	AnchorTurnID string `json:"anchor_turn_id,omitempty"`
	// BaseHash 是覆盖 AnchorTurnID 之前内容的那份更早检查点的 PrefixHash（无则空）。
	// 它只作为链式哈希的种子被信任，不重算：更早区间由写入侧的失效逻辑
	// （invalidateDependentCheckpoints）守护。
	BaseHash string `json:"base_hash,omitempty"`
	// CoveredTurns 是被覆盖的轮次数，供装载侧决定需要回读多少行来校验前缀。
	CoveredTurns int `json:"covered_turns,omitempty"`
	// Source 记录产生检查点的路径，用于诊断："agent_compaction"（Agent 上下文压缩）
	// 或 "smart_history"（Map-Reduce 增量摘要）。
	Source string `json:"source,omitempty"`
}

// 检查点来源标识（见 ContextCheckpoint.Source）。
const (
	ContextCheckpointSourceAgent = "agent_compaction"
	ContextCheckpointSourceSmart = "smart_history"
	contextCheckpointHashVersion = "zilan-ctxcp-v1"
)

// Usable 报告检查点是否可被复用：摘要非空，且带有可重算校验的前缀身份。
func (c *ContextCheckpoint) Usable() bool {
	return c != nil && strings.TrimSpace(c.Summary) != "" && c.PrefixHash != ""
}

// Value implements the driver.Valuer interface for database serialization.
// 空摘要（以及 nil）存 NULL：它与「这一轮从未被压缩过」在语义上不可区分，
// 读取侧只认 NULL/非 NULL。指针接收者与 Message 上其它 jsonb 列保持一致。
func (c *ContextCheckpoint) Value() (driver.Value, error) {
	if c == nil || strings.TrimSpace(c.Summary) == "" {
		return nil, nil
	}
	return json.Marshal(c)
}

// Scan implements the sql.Scanner interface for database deserialization.
func (c *ContextCheckpoint) Scan(value any) error {
	if value == nil {
		*c = ContextCheckpoint{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("types: cannot scan context checkpoint from unsupported type")
	}
	if len(b) == 0 {
		*c = ContextCheckpoint{}
		return nil
	}
	return json.Unmarshal(b, c)
}

// ContextCheckpointSink 为 Agent 引擎持久化压缩检查点。与 SteerSink 同理声明在 types
// 里：引擎与服务两侧都需要这个形状，而两者不能互相 import。
type ContextCheckpointSink interface {
	// SaveContextCheckpoint 把 checkpoint 写到 assistant 消息 turnMessageID 上，
	// 覆盖该行既有的检查点。
	SaveContextCheckpoint(ctx context.Context, turnMessageID string, checkpoint *ContextCheckpoint) error
}

// ---------------------------------------------------------------------------
// 前缀哈希（FR-10 规则 3：前缀严格一致）
// ---------------------------------------------------------------------------

// StoredMessage 是参与前缀哈希的一条持久化消息的最小身份：轮次身份（RequestID）、
// 角色与内容。
//
// 刻意不含时间戳、不含 token 计数：换 tokenizer、时钟漂移或行更新时间变化都不该让
// 检查点失效，只有内容本身的变化才应该。
type StoredMessage struct {
	TurnID  string
	Role    string
	Content string
}

// CheckpointChainSeed 返回链式前缀哈希的种子。
//
// promptVersion 由调用方给出（Agent 侧混入摘要占位标记与摘要提示词模板的版本）：
// 摘要信封或提示词模板一旦变化，旧摘要就是按旧规格生成的，必须整体失效。
// baseHash 是更早一份检查点的 PrefixHash（无则空），anchorTurnID 是被覆盖区间的起始
// 轮次身份——它使「同样的内容出现在不同位置」得到不同的哈希。
func CheckpointChainSeed(promptVersion, baseHash, anchorTurnID string) string {
	return checkpointHash(contextCheckpointHashVersion, promptVersion, baseHash, anchorTurnID)
}

// FoldStoredMessages 把若干持久化消息按时间序折进链式哈希，返回新的链值。
//
// 链式（而不是对整段区间一次性求哈希）使检查点可以增量延伸：新一轮压缩只需把新覆盖
// 的轮次折进上一份检查点的 PrefixHash，无需回读更早的内容。
func FoldStoredMessages(chain string, messages []StoredMessage) string {
	parts := make([]string, 0, len(messages)*3+1)
	parts = append(parts, chain)
	for _, m := range messages {
		parts = append(parts, m.TurnID, m.Role, sha256Hex(m.Content))
	}
	return checkpointHash(parts...)
}

// checkpointHash 对字段做长度前缀串联后求 sha256。长度前缀使字段边界不可伪造：
// ("ab","c") 与 ("a","bc") 不会碰撞。
func checkpointHash(parts ...string) string {
	h := sha256.New()
	var buf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(buf[:], uint64(len(p)))
		h.Write(buf[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

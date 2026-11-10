package compaction

// 外部产物必须活过压缩（PRD FR-06 规则 6）。
//
// 丢掉"我已经写出 Wiki 页面 X"这段历史后，模型就失去了自己已产出哪些产物的记录，
// 于是开始重建磁盘/知识库中已经存在的东西——对需要分块写入的大页面，重建正是那个永不
// 终止的循环。LLM 摘要本应承载这一点，但它是散文，而标识列表恰是摘要器最先丢掉的
// 细节。所以这里机械地抽取标识、附加到每份摘要上，并由下一份摘要继承。
//
// 收集期分两个集合，渲染时也是两个列表：写过的产物即使也被读过，也只作为"已产出"上报。

import (
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

const (
	producedTag   = "produced-artifacts"
	referencedTag = "referenced-artifacts"

	// maxTrackedArtifacts 限制该块能给后续每次请求增加的体积。触及更多产物的会话丢弃
	// 最旧的条目，而不是无上限地撑大提示词。
	maxTrackedArtifacts = 50
)

// artifactSpec 描述一个工具的哪些参数标识外部产物，以及该工具是否产生/改写外部状态。
type artifactSpec struct {
	keys    []string
	written bool
}

// artifactSpecs 覆盖 Zilan Agent 中会触及外部状态或产生可复用资源的工具。
// written=true 的工具改写知识库/工单等外部状态，压缩后模型必须知道它们已经发生过，
// 否则会重复执行（重复建页、重复覆盖）。
var artifactSpecs = map[string]artifactSpec{
	agenttools.ToolWikiWritePage:      {keys: []string{"slug", "title"}, written: true},
	agenttools.ToolWikiReplaceText:    {keys: []string{"slug"}, written: true},
	agenttools.ToolWikiRenamePage:     {keys: []string{"new_slug", "slug"}, written: true},
	agenttools.ToolWikiDeletePage:     {keys: []string{"slug"}, written: true},
	agenttools.ToolWikiFlagIssue:      {keys: []string{"slug"}, written: true},
	agenttools.ToolWikiUpdateIssue:    {keys: []string{"issue_id"}, written: true},
	agenttools.ToolExecuteSkillScript: {keys: []string{"script_path"}, written: true},
	agenttools.ToolWikiReadPage:       {keys: []string{"slug", "slugs"}},
	agenttools.ToolWikiReadSourceDoc:  {keys: []string{"knowledge_id"}},
	agenttools.ToolGetDocumentInfo:    {keys: []string{"knowledge_id", "knowledge_ids"}},
	agenttools.ToolWebFetch:           {keys: []string{"url"}},
	agenttools.ToolReadSkill:          {keys: []string{"skill_name"}},
}

// artifactOps 是被压缩掉的历史所触及的外部产物集合。收集期把读与写分开，以便后一次
// 写操作能把先前只被读过的产物重新归类。
type artifactOps struct {
	read    []string
	written []string
}

func appendUnique(list []string, path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return list
	}
	for _, existing := range list {
		if existing == path {
			return list
		}
	}
	if len(list) >= maxTrackedArtifacts {
		list = list[len(list)-maxTrackedArtifacts+1:]
	}
	return append(list, path)
}

// extractArtifacts 收集被摘要掉的消息所读写的外部产物。previousSummary 携带从更早一次
// 压缩继承下来的块——摘要不参与其后继摘要的输入，所以那份块已经不在被摘要的消息范围内。
func extractArtifacts(previousSummary string, groups ...[]chat.Message) artifactOps {
	var ops artifactOps
	ops.inherit(previousSummary)
	for _, group := range groups {
		for _, msg := range group {
			ops.inherit(messageText(msg))
			for _, tc := range msg.ToolCalls {
				spec, ok := artifactSpecs[tc.Function.Name]
				if !ok {
					continue
				}
				for _, id := range toolCallArtifacts(tc.Function.Arguments, spec.keys) {
					if spec.written {
						ops.written = appendUnique(ops.written, id)
					} else {
						ops.read = appendUnique(ops.read, id)
					}
				}
			}
		}
	}
	return ops
}

// inherit 把一个已渲染的块折回收集集合。它的"已产出"列表已经过 resolve()，因此原样
// 作为 written 收回。
func (o *artifactOps) inherit(content string) {
	if content == "" {
		return
	}
	inheritedRead, inheritedWritten := parseArtifactBlock(content)
	for _, p := range inheritedRead {
		o.read = appendUnique(o.read, p)
	}
	for _, p := range inheritedWritten {
		o.written = appendUnique(o.written, p)
	}
}

// resolve 把两个收集集合折叠成给模型看的样子：凡是被写过的都算"已产出"，只有从未被
// 写过的才作为"已读取"列出。
func (o artifactOps) resolve() (read, produced []string) {
	for _, p := range o.written {
		produced = appendUnique(produced, p)
	}
	for _, p := range o.read {
		if !contains(produced, p) {
			read = appendUnique(read, p)
		}
	}
	return read, produced
}

func contains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// toolCallArtifacts 从一次工具调用的参数中取出 keys 对应的标识。参数畸形或被截断时
// 不贡献任何内容。值可以是字符串，也可以是字符串数组（如 slugs / knowledge_ids）。
func toolCallArtifacts(arguments string, keys []string) []string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return nil
	}
	var out []string
	for _, key := range keys {
		switch v := parsed[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, s)
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

// format 渲染附加到摘要末尾的块；什么都没触及时返回 ""。
// 标签内只放标识本身（每行一个），说明文字放在标签外，否则 parseArtifactBlock 会把
// 说明当成产物继承下去。
func (o artifactOps) format() string {
	read, produced := o.resolve()
	if len(read) == 0 && len(produced) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[以下清单由上下文压缩机制机械抽取，用于避免重复产出或重复检索。]")
	writeTagged(&sb, producedTag,
		"已产出/已改写的外部产物（已存在，不要重新创建）：", produced)
	writeTagged(&sb, referencedTag,
		"已读取/已检索的资源（结论见上方摘要，不要重复检索）：", read)
	return sb.String()
}

func writeTagged(sb *strings.Builder, tag, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n\n%s\n<%s>\n%s\n</%s>", label, tag, strings.Join(items, "\n"), tag)
}

// parseArtifactBlock 读回 format 写下的内容，使连续多次压缩能够累积，而不是每次都忘掉
// 上一次。
func parseArtifactBlock(content string) (read, produced []string) {
	return parseTagged(content, referencedTag), parseTagged(content, producedTag)
}

func parseTagged(content, tag string) []string {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	start := strings.Index(content, open)
	if start < 0 {
		return nil
	}
	start += len(open)
	end := strings.Index(content[start:], closeTag)
	if end < 0 {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(content[start:start+end], "\n") {
		paths = appendUnique(paths, line)
	}
	return paths
}

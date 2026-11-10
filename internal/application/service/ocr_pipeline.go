// FR-13 OCR 流水线可配置化：参数的校验、自描述导出，以及把知识库配置摊平
// 成 parser_engine_overrides 送到 docreader（复用既有通道，不改 proto）。
//
// 校验分两层，对应两种失败代价：
//
//	保存时（ValidateOCRPipelineConfig）—— 严格拒绝。类型不对、超出取值范围、
//	或「开了前处理模式却一个步骤都没启用」都在这一刻报错，避免把跑不起来的
//	配置写进库。
//	运行时（Sanitize*）—— 越界的项被丢弃并回落字段声明的默认值，同时打一条
//	警告日志。绝不 clamp：悄悄把 601 改成 600 会让用户以为自己的配置生效了。
//
// 存量兼容（PRD §5.4）：本文件不产生任何重解析信号，OCR 流水线相关配置也
// 没有进入 reparseFileTypes 白名单，因此改开关不会动到已入库的文档。

package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// OCRPipelineOverrideKey 是 OCR 流水线 id 在 parser_engine_overrides 里的键名。
// 它必须是合法的 Python 标识符：overrides 会经 `**kwargs` 展开传给 parser。
const OCRPipelineOverrideKey = "ocr_pipeline"

// ListOCRPipelines 导出全部流水线的自描述，供前端渲染配置面板。
// 每条都带 Note 字段，内容是「仅对新解析文档生效，存量文档需手动重新解析」，
// 这是 PRD §5.4 要求配置页必须展示的提示。
func ListOCRPipelines() []types.OCRPipelineSpec {
	specs := types.OCRPipelineSpecs()
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	return specs
}

// ValidateOCRPipelineParams 校验一张流水线参数表能否被该流水线执行。
// 未被该流水线声明的键直接忽略（旧版本存下的配置在新版本上仍然可读），
// 声明过的键则逐项检查类型与取值范围。
func ValidateOCRPipelineParams(pipelineID string, params map[string]any) error {
	id := types.NormalizeOCRPipelineID(types.OCRPipelineID(pipelineID))
	fields := types.OCRPipelineFields(id)
	for key, raw := range params {
		bare := strings.TrimPrefix(key, id.String()+".")
		field, ok := types.OCRFieldLookup(fields, bare)
		if !ok {
			continue
		}
		if _, err := types.ValidateOCRPipelineFieldValue(field, raw); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePreprocessParams 校验前处理参数表（去噪/纠偏/对比度/留白）。
func ValidatePreprocessParams(params map[string]any) error {
	fields := types.PreprocessFields()
	for key, raw := range params {
		field, ok := types.OCRFieldLookup(fields, key)
		if !ok {
			continue
		}
		if _, err := types.ValidateOCRPipelineFieldValue(field, raw); err != nil {
			return err
		}
	}
	return nil
}

// EvaluateOCRPipelineRules 按声明顺序求值一组规则，返回全部被违反的规则。
// 一次报全部而不是只报第一条，是为了让配置面板能一次性告诉用户要改哪几处。
//
// 参数表里没提到的字段读它声明的默认值 —— 与运行时一致：一个在该字段出现
// 之前就存下来的知识库，不会因为「没提到」而被判定成关掉了什么。
func EvaluateOCRPipelineRules(
	pipelineID types.OCRPipelineID,
	rules []types.OCRPipelineRules,
	fields []types.OCRFieldDef,
	params map[string]any,
) []*types.OCRPipelineValidationError {
	broken := make([]*types.OCRPipelineValidationError, 0, len(rules))
	for _, rule := range rules {
		if len(rule.AtLeastOne) == 0 || rule.MessageKey == "" {
			continue
		}
		if !ocrRuleApplies(rule, fields, params, pipelineID.String()) {
			continue
		}
		satisfied := false
		for _, key := range rule.AtLeastOne {
			if ocrBoolParam(params, fields, key, pipelineID.String()) {
				satisfied = true
				break
			}
		}
		if satisfied {
			continue
		}
		field := rule.Field
		if field == "" {
			field = rule.AtLeastOne[0]
		}
		broken = append(broken, &types.OCRPipelineValidationError{
			MessageKey: rule.MessageKey,
			Field:      field,
			Message:    "启用 OCR 前处理后，至少需要打开一个前处理步骤（去噪 / 纠偏 / 对比度增强）",
		})
	}
	return broken
}

// ocrRuleApplies 判断一条规则的前提条件是否成立。
func ocrRuleApplies(
	rule types.OCRPipelineRules,
	fields []types.OCRFieldDef,
	params map[string]any,
	pipelineID string,
) bool {
	if rule.WhenField == "" {
		return true
	}
	current := ocrStringParam(params, fields, rule.WhenField, pipelineID)
	if len(rule.WhenValues) == 0 {
		return current != ""
	}
	for _, want := range rule.WhenValues {
		if strings.EqualFold(want, current) {
			return true
		}
	}
	return false
}

// ocrParamValue 在合并后的参数表里按裸键取值：先找流水线 slot 键，再找裸键。
// 前处理参数（如 denoise.enabled）与流水线参数共用同一张合并表。
func ocrParamValue(params map[string]any, key, pipelineID string) (any, bool) {
	if params == nil {
		return nil, false
	}
	if v, ok := params[types.SlotKey(pipelineID, key)]; ok && v != nil {
		return v, true
	}
	if v, ok := params[key]; ok && v != nil {
		return v, true
	}
	return nil, false
}

// ocrBoolParam 取一个布尔参数的生效值，缺失时回落到声明的默认值。
func ocrBoolParam(params map[string]any, fields []types.OCRFieldDef, key, pipelineID string) bool {
	raw, ok := ocrParamValue(params, key, pipelineID)
	if !ok {
		return ocrDefaultBool(fields, key)
	}
	field, declared := types.OCRFieldLookup(fields, key)
	if !declared {
		field = types.OCRFieldDef{Key: key, Type: types.OCRFieldTypeBool}
	}
	v, err := types.ValidateOCRPipelineFieldValue(field, raw)
	if err != nil {
		return ocrDefaultBool(fields, key)
	}
	b, _ := v.(bool)
	return b
}

func ocrStringParam(params map[string]any, fields []types.OCRFieldDef, key, pipelineID string) string {
	raw, ok := ocrParamValue(params, key, pipelineID)
	if !ok {
		return ""
	}
	s, _ := raw.(string)
	return strings.TrimSpace(s)
}

func ocrDefaultBool(fields []types.OCRFieldDef, key string) bool {
	if field, ok := types.OCRFieldLookup(fields, key); ok {
		b, _ := field.Default.(bool)
		return b
	}
	return false
}

// BrokenOCRPipelineRules 列出一份知识库配置违反的全部规则。
// 前处理参数的键不在流水线参数表里，因此求值前把三处合并成一张表：
// 流水线参数、前处理模式、前处理步骤开关。
func BrokenOCRPipelineRules(
	pipelineID string,
	params map[string]any,
	preprocessMode string,
	preprocessParams map[string]any,
) []*types.OCRPipelineValidationError {
	id := types.NormalizeOCRPipelineID(types.OCRPipelineID(pipelineID))
	merged := make(map[string]any, len(params)+len(preprocessParams)+1)
	for k, v := range params {
		merged[k] = v
	}
	for k, v := range preprocessParams {
		merged[k] = v
	}
	merged[types.PreprocessModeKey] = types.NormalizePreprocessMode(preprocessMode)

	// 规则里的前处理键是裸名，字段声明来自前处理字段清单，与流水线字段合并查找。
	fields := append(append([]types.OCRFieldDef{}, types.OCRPipelineFields(id)...), types.PreprocessFields()...)
	return EvaluateOCRPipelineRules(id, types.OCRPipelineRulesFor(id), fields, merged)
}

// ValidateOCRPipelineConfig 是知识库创建/更新路径上的校验入口：一个从未打开
// 过配置面板的调用方（API 客户端、导入脚本）也不能存下跑不起来的组合。
//
// 流水线 id 按运行时的方式解析而不是照抄字段原值：FR-13 之前创建的知识库
// 根本没有这个字段，那不能让校验变成空操作。
func ValidateOCRPipelineConfig(ctx context.Context, cfg *types.ImageProcessingConfig) error {
	if cfg == nil {
		return nil
	}
	id := types.ResolveOCRPipelineID(cfg)
	if err := ValidateOCRPipelineParams(id.String(), cfg.OCRPipelineParams); err != nil {
		logger.Warnf(ctx, "[ocr.pipeline] rejecting pipeline %q params: %v", id, err)
		return err
	}
	mode := types.ResolvePreprocessMode(cfg)
	if err := ValidatePreprocessParams(cfg.PreprocessParams); err != nil {
		logger.Warnf(ctx, "[ocr.pipeline] rejecting preprocess params: %v", err)
		return err
	}
	broken := BrokenOCRPipelineRules(id.String(), cfg.OCRPipelineParams, mode, cfg.PreprocessParams)
	if len(broken) > 0 {
		logger.Warnf(ctx, "[ocr.pipeline] rejecting pipeline %q: %d broken rule(s), first=%q",
			id, len(broken), broken[0].MessageKey)
		return broken[0]
	}
	return nil
}

// InjectOCRPipelineOverrides 把知识库的 OCR 流水线配置摊平写进
// parser_engine_overrides，经既有通道传到 docreader（不改 proto）。
//
// 键名规则：
//
//	ocr_pipeline                  流水线 id（manual / smart）
//	<pipeline>.<key>              流水线参数，用 types.SlotKey 加命名空间
//	preprocess.mode               前处理模式
//	preprocess.<key>              前处理参数，如 preprocess.denoise.sigma
//
// 只下发知识库**显式配置过**的参数：没配的键直接缺席，由 docreader 回落到
// 它自己的默认值（服务环境变量 > 内置默认）。这样运维用
// DOCREADER_PDF_RENDER_DPI 之类调过的部署，不会被这里声明的默认值悄悄盖掉，
// 手动流水线的行为也就与 FR-13 之前逐字一致。
//
// 越界或类型不对的参数在这里被丢弃（回落默认值）并打警告，不 clamp、不报错：
// 解析一份文档不该因为一个存坏的旋钮而失败。
func InjectOCRPipelineOverrides(ctx context.Context, kb *types.KnowledgeBase, overrides map[string]string) map[string]string {
	out := overrides
	if out == nil {
		out = make(map[string]string)
	}
	if kb == nil {
		return out
	}
	cfg := kb.ImageProcessingConfig
	id := types.ResolveOCRPipelineID(&cfg)
	mode := types.ResolvePreprocessMode(&cfg)

	out[OCRPipelineOverrideKey] = id.String()
	out[types.PreprocessModeKey] = mode

	for key, value := range sanitizedOCRPipelineParams(ctx, id, cfg.OCRPipelineParams) {
		out[key] = ocrOverrideString(value)
	}
	for key, value := range sanitizedPreprocessParams(ctx, cfg.PreprocessParams) {
		out[types.PreprocessParamPrefix+key] = ocrOverrideString(value)
	}
	return out
}

// sanitizedOCRPipelineParams 返回一份可直接下发的流水线参数：只保留该流水线
// 声明过且通过校验的键，值规范化成字段声明的类型。未配置的键不补默认值
// （见 InjectOCRPipelineOverrides 的说明）。
func sanitizedOCRPipelineParams(ctx context.Context, id types.OCRPipelineID, params map[string]any) map[string]any {
	fields := types.OCRPipelineFields(id)
	sanitized, rejected := types.SanitizeOCRPipelineParams(fields, params, id.String())
	for _, key := range rejected {
		logger.Warnf(ctx, "[ocr.pipeline] param %q out of range or wrong type for pipeline %q; falling back to its default",
			key, id)
		delete(sanitized, key)
	}
	// 只下发该流水线声明过的键，避免把别的流水线的残留参数带进解析进程。
	declared := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if field.Type == types.OCRFieldTypeNote {
			continue
		}
		declared[types.SlotKey(id.String(), field.Key)] = struct{}{}
	}
	out := make(map[string]any, len(sanitized))
	for key, value := range sanitized {
		if _, ok := declared[key]; ok {
			out[key] = value
		}
	}
	return out
}

// sanitizedPreprocessParams 同上，作用于前处理参数（键为裸名）。
func sanitizedPreprocessParams(ctx context.Context, params map[string]any) map[string]any {
	fields := types.PreprocessFields()
	sanitized, rejected := types.SanitizeOCRPipelineParams(fields, params, "")
	for _, key := range rejected {
		logger.Warnf(ctx, "[ocr.pipeline] preprocess param %q out of range or wrong type; falling back to its default", key)
		delete(sanitized, key)
	}
	return sanitized
}

// ocrOverrideString 把一个规范化后的参数值序列化成 override 字符串。
func ocrOverrideString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

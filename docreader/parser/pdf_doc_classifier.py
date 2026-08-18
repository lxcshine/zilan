"""Document type classification for adaptive parsing (§5.3).

Classifies PDFs into types (academic paper, contract, financial report,
resume, technical manual, general) using lightweight rule-based heuristics
that work on the text layer without requiring a trained model.

Each type triggers a different parsing strategy:
  - academic: preserve citation chains, formulas, figure captions,
              strengthen dual-column layout detection
  - contract: extract clause numbering, parties, key dates, signature pages
  - financial: table-priority parsing, preserve numeric precision,
               link year-over-year data
  - resume: structured field extraction (name/company/skills/projects)
  - manual: preserve section hierarchy, code blocks, diagrams
  - general: default balanced strategy
"""

import logging
import re
from dataclasses import dataclass
from typing import List, Optional

logger = logging.getLogger(__name__)


@dataclass
class DocClassification:
    """Result of document type classification."""
    doc_type: str  # academic, contract, financial, resume, manual, general
    confidence: float
    signals: List[str]  # human-readable reasons for the classification
    strategy: str  # parsing strategy name


# Type names
TYPE_ACADEMIC = "academic"
TYPE_CONTRACT = "contract"
TYPE_FINANCIAL = "financial"
TYPE_RESUME = "resume"
TYPE_MANUAL = "manual"
TYPE_GENERAL = "general"


# --- Academic paper signals ---
_ACADEMIC_PATTERNS = [
    (re.compile(r"\barXiv:\s*\S+", re.I), "arXiv identifier"),
    (re.compile(r"\bdoi:\s*10\.\d{4,}/", re.I), "DOI reference"),
    (re.compile(r"\b(?:Abstract|Keywords?|Introduction|References?|Conclusion)\b", re.I), "academic section headings"),
    (re.compile(r"\[\d+\]\s+\w+", re.I), "citation reference [n]"),
    (re.compile(r"\b(?:et al\.|ibid\.|op\. cit\.)", re.I), "academic citation style"),
    (re.compile(r"\b(?:IEEE|ACM|Springer|Elsevier|Wiley)\b", re.I), "publisher name"),
    (re.compile(r"\b(?:equation|theorem|lemma|corollary|proof)\b", re.I), "mathematical structure"),
    (re.compile(r"Figure\s+\d+", re.I), "figure reference"),
    (re.compile(r"Table\s+\d+", re.I), "table reference"),
]

# --- Contract signals ---
_CONTRACT_PATTERNS = [
    (re.compile(r"\b(?:甲方|乙方|丙方|Party A|Party B|甲方（?出租人|承租方|发包方|承包方)\b"), "contracting parties"),
    (re.compile(r"\b(?:合同编号|Contract No\.?|Agreement No\.?)", re.I), "contract number"),
    (re.compile(r"\b(?:兹|鉴于|WHEREAS|NOW THEREFORE|特此)\b", re.I), "contract preamble"),
    (re.compile(r"\b(?:第[一二三四五六七八九十百千\d]+条|Article\s+\d+|Clause\s+\d+)\b", re.I), "clause numbering"),
    (re.compile(r"\b(?:签署日期|签订日期|Date of Signing|Execution Date)\b", re.I), "signature date"),
    (re.compile(r"\b(?:盖章|签字|signature|seal)\b", re.I), "signature/seal"),
    (re.compile(r"\b(?:违约责任|赔偿责任|保密义务|知识产权|不可抗力)\b"), "contract clauses"),
    (re.compile(r"\b(?:本合同|本协议|本约定|This Agreement|This Contract)\b", re.I), "contract self-reference"),
]

# --- Financial report signals ---
_FINANCIAL_PATTERNS = [
    (re.compile(r"\b(?:资产负债表|利润表|现金流量表|所有者权益变动表)\b"), "financial statement names"),
    (re.compile(r"\b(?:Balance Sheet|Income Statement|Cash Flow Statement|Statement of Changes in Equity)\b", re.I), "financial statement (EN)"),
    (re.compile(r"\b(?:营业收入|净利润|总资产|净资产|每股收益|ROE|ROA)\b"), "financial metrics"),
    (re.compile(r"\b(?:Revenue|Net Income|Total Assets|EPS|EBITDA|Operating Profit)\b", re.I), "financial metrics (EN)"),
    (re.compile(r"\b(?:同比|环比|year.over.year|YoY|Q[1-4]\s*20\d{2})\b", re.I), "year-over-year comparison"),
    (re.compile(r"\b(?:审计报告|Audit Report|独立核数师报告)\b", re.I), "audit report"),
    (re.compile(r"\b(?:万元|百万元|亿元|千元)\b"), "Chinese currency units"),
    (re.compile(r"\b(?:USD|EUR|CNY|RMB|\$[\d,]+)\b"), "currency symbols"),
    (re.compile(r"\(\s*-?[\d,.]+\s*\)|-?[\d,.]+\s*%"), "financial numbers/percentages"),
]

# --- Resume signals ---
_RESUME_PATTERNS = [
    (re.compile(r"\b(?:姓名|Name|性别|Gender|出生年月|Date of Birth|国籍|Nationality)\b", re.I), "personal info fields"),
    (re.compile(r"\b(?:教育背景|Education|工作经历|Work Experience|项目经历|Project Experience)\b", re.I), "resume sections"),
    (re.compile(r"\b(?:技能|Skills|语言能力|Language Proficiency|证书|Certifications)\b", re.I), "resume sections (cont.)"),
    (re.compile(r"\b(?:本科|硕士|博士|Bachelor|Master|Ph\.?D\.?|MBA)\b", re.I), "education degrees"),
    (re.compile(r"\b(?:公司|Company|职位|Position|职责|Responsibilities)\b", re.I), "work experience fields"),
    (re.compile(r"\b\d{4}\.\s*\d{1,2}\s*[-–—]\s*(?:至今|present|now|\d{4})\b", re.I), "date ranges"),
    (re.compile(r"^\s*(?:男|女)\s*$", re.MULTILINE), "gender field"),
]

# --- Technical manual signals ---
_MANUAL_PATTERNS = [
    (re.compile(r"\b(?:步骤|Step\s+\d+|Procedure|Instructions?)\b", re.I), "step/procedure"),
    (re.compile(r"\b(?:注意|Warning|Caution|Danger|重要提示)\b", re.I), "safety notice"),
    (re.compile(r"\b(?:配置|Configuration|设置|Settings?|参数|Parameters?)\b", re.I), "configuration"),
    (re.compile(r"\b(?:故障|Troubleshoot|诊断|Diagnostic|排查)\b", re.I), "troubleshooting"),
    (re.compile(r"```"), "code block"),
    (re.compile(r"\b(?:API|SDK|HTTP|REST|JSON|XML|YAML|Dockerfile)\b", re.I), "technical terms"),
    (re.compile(r"\b(?:安装|Installation|部署|Deployment|升级|Upgrade)\b", re.I), "installation/deployment"),
    (re.compile(r"^\s*(?:\d+\.\d+)+\s+\w", re.MULTILINE), "numbered section hierarchy"),
]


def classify_document(text: str, page_count: int = 0, metadata: dict = None) -> DocClassification:
    """Classify a document by type using rule-based heuristics.

    Args:
        text: The first ~5000 chars of the document text (or full text).
        page_count: Total page count (used as auxiliary signal).
        metadata: Document metadata from the parser.

    Returns:
        DocClassification with type, confidence, signals, and strategy.
    """
    if not text or len(text.strip()) < 50:
        return DocClassification(
            doc_type=TYPE_GENERAL,
            confidence=0.5,
            signals=["insufficient text for classification"],
            strategy="general",
        )

    # Sample: first 5000 chars (enough for classification, fast)
    sample = text[:5000]

    scores = {
        TYPE_ACADEMIC: _score_type(sample, _ACADEMIC_PATTERNS),
        TYPE_CONTRACT: _score_type(sample, _CONTRACT_PATTERNS),
        TYPE_FINANCIAL: _score_type(sample, _FINANCIAL_PATTERNS),
        TYPE_RESUME: _score_type(sample, _RESUME_PATTERNS),
        TYPE_MANUAL: _score_type(sample, _MANUAL_PATTERNS),
    }

    # Pick the highest-scoring type
    best_type = max(scores, key=scores.get)
    best_score = scores[best_type]
    second_score = sorted(scores.values(), reverse=True)[1] if len(scores) > 1 else 0

    # Confidence: normalized score with margin over second-best
    confidence = min(1.0, best_score / 3.0)  # 3+ signals = high confidence
    margin = best_score - second_score
    if margin < 0.5 and best_score < 2:
        # Weak signal — default to general
        return DocClassification(
            doc_type=TYPE_GENERAL,
            confidence=0.5,
            signals=["weak classification signals"],
            strategy="general",
        )

    # Collect the matched signals for the winning type
    pattern_map = {
        TYPE_ACADEMIC: _ACADEMIC_PATTERNS,
        TYPE_CONTRACT: _CONTRACT_PATTERNS,
        TYPE_FINANCIAL: _FINANCIAL_PATTERNS,
        TYPE_RESUME: _RESUME_PATTERNS,
        TYPE_MANUAL: _MANUAL_PATTERNS,
    }

    signals = []
    for pattern, label in pattern_map[best_type]:
        if pattern.search(sample):
            signals.append(label)

    strategy = _strategy_for_type(best_type)

    return DocClassification(
        doc_type=best_type,
        confidence=confidence,
        signals=signals[:5],  # top 5 signals
        strategy=strategy,
    )


def _score_type(text: str, patterns: list) -> float:
    """Count how many patterns match in the text."""
    return sum(1 for pattern, _ in patterns if pattern.search(text))


def _strategy_for_type(doc_type: str) -> str:
    """Map document type to parsing strategy name."""
    strategies = {
        TYPE_ACADEMIC: "academic",
        TYPE_CONTRACT: "contract",
        TYPE_FINANCIAL: "financial",
        TYPE_RESUME: "resume",
        TYPE_MANUAL: "manual",
        TYPE_GENERAL: "general",
    }
    return strategies.get(doc_type, "general")


def get_strategy_config(strategy: str) -> dict:
    """Return type-specific parsing configuration.

    These configs control how the PDF parser processes the document:
    - table_priority: extract tables before text
    - formula_extraction: attempt LaTeX OCR
    - dual_column_enhanced: stronger column detection
    - preserve_citations: keep reference list intact
    - clause_extraction: extract numbered clauses
    - field_extraction: extract structured fields
    """
    configs = {
        "academic": {
            "table_priority": True,
            "formula_extraction": True,
            "dual_column_enhanced": True,
            "preserve_citations": True,
            "preserve_captions": True,
            "clause_extraction": False,
            "field_extraction": False,
        },
        "contract": {
            "table_priority": False,
            "formula_extraction": False,
            "dual_column_enhanced": False,
            "preserve_citations": False,
            "preserve_captions": False,
            "clause_extraction": True,
            "field_extraction": True,
        },
        "financial": {
            "table_priority": True,
            "formula_extraction": False,
            "dual_column_enhanced": False,
            "preserve_citations": False,
            "preserve_captions": True,
            "clause_extraction": False,
            "field_extraction": True,
        },
        "resume": {
            "table_priority": False,
            "formula_extraction": False,
            "dual_column_enhanced": False,
            "preserve_citations": False,
            "preserve_captions": False,
            "clause_extraction": False,
            "field_extraction": True,
        },
        "manual": {
            "table_priority": True,
            "formula_extraction": False,
            "dual_column_enhanced": False,
            "preserve_citations": False,
            "preserve_captions": True,
            "clause_extraction": False,
            "field_extraction": False,
        },
        "general": {
            "table_priority": True,
            "formula_extraction": True,
            "dual_column_enhanced": True,
            "preserve_citations": True,
            "preserve_captions": True,
            "clause_extraction": False,
            "field_extraction": False,
        },
    }
    return configs.get(strategy, configs["general"])

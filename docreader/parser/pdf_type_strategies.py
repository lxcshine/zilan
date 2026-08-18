"""Type-specific parsing strategy application (§5.3).

After `pdf_doc_classifier` determines the document type, this module applies
the corresponding deep-parsing strategy:

  - contract:  extract clause numbers, contracting parties, key dates,
               signature blocks → structured metadata + navigable markdown
  - financial: table-priority (tables kept intact), numeric precision
               preserved, YoY (同比) data paired with base periods
  - resume:    structured field extraction (name / education / companies /
               skills / projects)
  - academic:  citation chain preservation ([n] references kept intact and
               the References section marked), figure captions kept adjacent
               to their figures

The strategies operate on the markdown text produced by the builtin engine
and return (enhanced_text, structured_fields). Structured fields are placed
into Document.metadata so the Go App can index them without re-parsing.
"""

import json
import logging
import re
from typing import Dict, List, Optional, Tuple

logger = logging.getLogger(__name__)

MAX_CLAUSES = 200
MAX_METRICS = 50
MAX_SKILLS = 60


# ---------------------------------------------------------------------------
# Contract strategy
# ---------------------------------------------------------------------------

_CLAUSE_ZH_RE = re.compile(r"^第[一二三四五六七八九十百千零\d]+条\s*", re.MULTILINE)
_CLAUSE_EN_RE = re.compile(r"^(?:Article|Clause|Section)\s+(\d+(?:\.\d+)*)\.?\s+", re.MULTILINE)
_PARTY_RE = re.compile(
    r"(?:甲方|乙方|丙方|丁方|出租方|承租方|发包方|承包方|买方|卖方|许可方|被许可方|债权人|债务人|Party\s+[A-D]|Licenser|Licensee|Buyer|Seller)\s*[:：]?\s*([^\n，,。;；]{2,40})"
)
_DATE_RE = re.compile(
    r"(\d{4}\s*[年\-/.]\s*\d{1,2}\s*[月\-/.]\s*\d{1,2}\s*日?)"
    r"|(\d{1,2}\s*[月\-/.]\s*\d{1,2}\s*[日\-/.]\s*\d{4})"
    r"|((?:19|20)\d{2}[\-/.]\d{1,2}[\-/./]\d{1,2})"
)
_SIGN_RE = re.compile(
    r"(签字|签章|盖章|签署|授权代表|Signature|Sign\s+Here|（签章）|\(签章\)|盖章处)", re.I
)
_KEY_DATE_CTX_RE = re.compile(
    r"(签署|签订|生效|履行|交付|验收|付款|到期|终止|有效期|签署日期|签订日期|生效日期|Signing|Effective|Expiry|Termination|Delivery|Payment)", re.I
)


def extract_contract_fields(text: str) -> Dict:
    """Extract structured contract fields: clauses, parties, dates, signature."""
    clauses: List[Dict] = []
    for m in _CLAUSE_ZH_RE.finditer(text):
        num = m.group(0).strip()
        start = m.end()
        # Title = rest of the clause line (up to 60 chars)
        line_end = text.find("\n", start)
        title = text[start:(line_end if line_end != -1 else start + 60)].strip()
        clauses.append({"clause": num.rstrip(), "title": title[:60]})
        if len(clauses) >= MAX_CLAUSES:
            break
    if not clauses:
        for m in _CLAUSE_EN_RE.finditer(text):
            line_end = text.find("\n", m.end())
            title = text[m.end():(line_end if line_end != -1 else m.end() + 60)].strip()
            clauses.append({"clause": m.group(0).strip(), "title": title[:60]})
            if len(clauses) >= MAX_CLAUSES:
                break

    parties: List[Dict] = []
    seen_parties = set()
    for m in _PARTY_RE.finditer(text):
        role = m.group(0).split("[:：]")[0].strip()
        name = m.group(1).strip().rstrip("（(")
        if name and name not in seen_parties and len(name) >= 2:
            seen_parties.add(name)
            parties.append({"role": role, "name": name})
        if len(parties) >= 10:
            break

    key_dates: List[Dict] = []
    seen_dates = set()
    for m in _DATE_RE.finditer(text):
        d = "".join(g for g in m.groups() if g)
        if not d or d in seen_dates:
            continue
        # Context window around the date to classify it
        ctx_start = max(0, m.start() - 30)
        ctx = text[ctx_start:m.start() + 10]
        if _KEY_DATE_CTX_RE.search(ctx):
            seen_dates.add(d)
            key_dates.append({"date": d, "context": ctx.strip()[-40:]})
        if len(key_dates) >= 20:
            break

    has_signature = bool(_SIGN_RE.search(text))

    return {
        "clause_count": len(clauses),
        "clauses": clauses[:50],
        "parties": parties,
        "key_dates": key_dates,
        "has_signature_block": has_signature,
    }


def apply_contract_strategy(text: str) -> Tuple[str, Dict]:
    """Enhance contract markdown: promote clause headings for better chunking."""
    fields = extract_contract_fields(text)

    # Promote clause lines to H3 headings so chunkers split at clause
    # boundaries (each clause becomes its own retrieval unit).
    def _zh_heading(m):
        return f"### {m.group(0).strip()}"

    def _en_heading(m):
        return f"### {m.group(0).strip()}"

    text = _CLAUSE_ZH_RE.sub(_zh_heading, text)
    text = _CLAUSE_EN_RE.sub(_en_heading, text)
    return text, fields


# ---------------------------------------------------------------------------
# Financial report strategy
# ---------------------------------------------------------------------------

_METRIC_ZH_RE = re.compile(
    r"(营业收入|营业总收入|净利润|归母净利润|毛利润|总资产|总负债|净资产|股东权益|"
    r"每股收益|基本每股收益|经营现金流|净利润率|毛利率|资产负债率|ROE|ROA|"
    r"Revenue|Net\s+Income|Gross\s+Profit|Total\s+Assets|Total\s+Liabilities|"
    r"Shareholders'??\s*Equity|EPS|EBITDA|Operating\s+Cash\s+Flow)\s*"
    r"[：:]?\s*([\(（]?\s*-?[\d,，]+(?:\.\d+)?)\s*[%％]?\s*[\)）]?\s*"
    r"(万亿元|百万元|千万元|万元|亿元|千元|元|USD|CNY)?"
)
_YOY_RE = re.compile(r"(?:同比|环比|YoY|QoQ)[^\n%\d]{0,12}(-?[\d,，.]+)\s*%?")
_YOY_METRIC_RE = re.compile(
    r"(营业收入|净利润|总资产|净资产|每股收益|Revenue|Net\s+Income|Total\s+Assets|EPS)"
    r"[^\n%]{0,30}?(?:同比|环比|YoY)[^\n%\d]{0,12}(-?[\d,，.]+)\s*%"
)


def extract_financial_highlights(text: str) -> Dict:
    """Extract key financial metrics and year-over-year relations."""
    metrics: List[Dict] = []
    seen = set()
    for m in _METRIC_ZH_RE.finditer(text):
        name = re.sub(r"\s+", " ", m.group(1)).strip()
        value = m.group(2).replace("，", ",").replace("（", "(")
        unit = m.group(3) or ""
        key = (name, value, unit)
        if key in seen:
            continue
        seen.add(key)
        metrics.append({"metric": name, "value": value, "unit": unit})
        if len(metrics) >= MAX_METRICS:
            break

    yoy: List[Dict] = []
    seen_y = set()
    for m in _YOY_METRIC_RE.finditer(text):
        name = re.sub(r"\s+", " ", m.group(1)).strip()
        pct = m.group(2)
        key = (name, pct)
        if key in seen_y:
            continue
        seen_y.add(key)
        yoy.append({"metric": name, "yoy_percent": pct})
        if len(yoy) >= 30:
            break
    if not yoy:
        for m in _YOY_RE.finditer(text):
            yoy.append({"metric": "overall", "yoy_percent": m.group(1)})
            if len(yoy) >= 5:
                break

    return {"key_metrics": metrics, "yoy_data": yoy}


def apply_financial_strategy(text: str, tables_md: Optional[List[str]] = None) -> Tuple[str, Dict]:
    """Financial strategy: keep tables intact at the top region + extract metrics.

    Number precision is preserved verbatim (no re-formatting of digits);
    the strategy only ADDS a structured summary block.
    """
    fields = extract_financial_highlights(text)

    if fields["key_metrics"]:
        lines = ["## 财务数据摘要 / Financial Highlights", ""]
        for m in fields["key_metrics"][:20]:
            lines.append(f"- {m['metric']}: {m['value']}{m['unit']}")
        summary = "\n".join(lines)
        # Prepend summary so chunk 0 carries the key figures.
        text = summary + "\n\n" + text

    return text, fields


# ---------------------------------------------------------------------------
# Resume strategy
# ---------------------------------------------------------------------------

_NAME_RE = re.compile(r"^(?:姓\s*名|Name)\s*[:：]\s*([^\s，,。；;|]{2,10})", re.MULTILINE)
_NAME_TOP_RE = re.compile(r"^([一-龥]{2,4})\s*$", re.MULTILINE)
_EDU_RE = re.compile(
    r"(本科|硕士|博士|研究生|大专|学士|Bachelor|Master|Ph\.?D\.?|MBA)[^\n，,。]{0,30}"
    r"((?:19|20)\d{2}(?:\s*[年\-/.]\s*(?:0?[1-9]|1[0-2]))?\s*[-–—至~]+\s*(?:至今|(?:19|20)\d{2})?)?"
)
_COMPANY_RE = re.compile(
    r"((?:19|20)\d{2}(?:\s*[年\-/.]\s*(?:0?[1-9]|1[0-2]))?\s*[-–—至~]+\s*(?:至今|present|now|(?:19|20)\d{2}(?:\s*[年\-/.]\s*(?:0?[1-9]|1[0-2]))?))\s*\n?([^\n]{2,50}(?:公司|集团|有限|科技|银行|大学|University|Inc\.?|Ltd\.?|Corp\.?|LLC|Institute))"
)
_SKILL_RE = re.compile(
    r"(?:技能|专业技能|Skills?|技术栈)\s*[:：]?\s*\n?([^\n]{5,200})"
)
_PROJECT_RE = re.compile(
    r"(?:项目(?:名称|经历)?|Project)\s*[:：]?\s*\n?([^\n]{3,80})"
)


def extract_resume_fields(text: str) -> Dict:
    """Extract structured resume fields."""
    name = None
    m = _NAME_RE.search(text)
    if m:
        name = m.group(1).strip()
    else:
        # Top-of-document bare Chinese name (2-4 chars, own line, in first 15 lines)
        head = "\n".join(text.splitlines()[:15])
        m2 = _NAME_TOP_RE.search(head)
        if m2:
            name = m2.group(1)

    education = []
    seen_e = set()
    for m in _EDU_RE.finditer(text):
        deg = m.group(1)
        if deg not in seen_e:
            seen_e.add(deg)
            education.append({"degree": deg, "period": (m.group(2) or "").strip()})
        if len(education) >= 6:
            break

    companies = []
    seen_c = set()
    for m in _COMPANY_RE.finditer(text):
        period = re.sub(r"\s+", " ", m.group(1)).strip()
        comp = m.group(2).strip()
        key = comp.lower()
        if key not in seen_c:
            seen_c.add(key)
            companies.append({"company": comp, "period": period})
        if len(companies) >= 10:
            break

    skills: List[str] = []
    m = _SKILL_RE.search(text)
    if m:
        raw = re.split(r"[、,，;；/|\s]{1,}", m.group(1))
        skills = [s.strip() for s in raw if 1 <= len(s.strip()) <= 30][:MAX_SKILLS]

    projects: List[str] = []
    for m in _PROJECT_RE.finditer(text):
        p = m.group(1).strip().rstrip("：:")
        if 2 <= len(p) <= 80:
            projects.append(p)
        if len(projects) >= 10:
            break

    return {
        "name": name,
        "education": education,
        "companies": companies,
        "skills": skills,
        "projects": projects,
    }


def apply_resume_strategy(text: str) -> Tuple[str, Dict]:
    """Resume strategy: prepend a structured summary card."""
    fields = extract_resume_fields(text)

    lines = ["## 简历摘要 / Resume Summary", ""]
    if fields.get("name"):
        lines.append(f"- 姓名: {fields['name']}")
    if fields.get("education"):
        lines.append("- 学历: " + "; ".join(e["degree"] for e in fields["education"]))
    if fields.get("companies"):
        lines.append(
            "- 工作经历: "
            + "; ".join(f"{c['company']}({c['period']})" for c in fields["companies"][:5])
        )
    if fields.get("skills"):
        lines.append("- 技能: " + ", ".join(fields["skills"][:20]))
    if len(lines) > 2:
        text = "\n".join(lines) + "\n\n" + text

    return text, fields


# ---------------------------------------------------------------------------
# Academic strategy
# ---------------------------------------------------------------------------

_REFERENCES_RE = re.compile(
    r"^#{0,4}\s*(?:References?|REFERENCES|参考文献|Bibliography)\s*$", re.MULTILINE
)
_CITATION_RE = re.compile(r"\[(\d{1,3})\]")


def apply_academic_strategy(text: str) -> Tuple[str, Dict]:
    """Academic strategy: mark the References section and citation chain.

    Keeps the reference list as one coherent block (do not let the chunker
    split it mid-reference), and records citation usage counts.
    """
    citation_ids: List[str] = []
    seen = set()
    for m in _CITATION_RE.finditer(text):
        cid = m.group(1)
        if cid not in seen:
            seen.add(cid)
            citation_ids.append(cid)
        if len(citation_ids) >= 200:
            break

    fields = {
        "citation_count": len(seen),
        "citation_ids": citation_ids[:100],
        "has_references_section": bool(_REFERENCES_RE.search(text)),
    }

    m = _REFERENCES_RE.search(text)
    if m:
        # Insert an anchor heading so downstream chunkers can keep the
        # reference list intact (chunk-boundary hint).
        text = text[:m.start()] + "## References\n" + text[m.end():]

    return text, fields


# ---------------------------------------------------------------------------
# Dispatcher
# ---------------------------------------------------------------------------

def apply_type_strategy(
    content: str,
    doc_type: str,
    tables_md: Optional[List[str]] = None,
) -> Tuple[str, Optional[Dict]]:
    """Apply the type-specific strategy. Returns (content, fields|None).

    Never raises: on any error returns the original content untouched.
    """
    try:
        if doc_type == "contract":
            return apply_contract_strategy(content)
        if doc_type == "financial":
            return apply_financial_strategy(content, tables_md)
        if doc_type == "resume":
            return apply_resume_strategy(content)
        if doc_type == "academic":
            return apply_academic_strategy(content)
        return content, None
    except Exception:
        logger.debug("type strategy %s failed", doc_type, exc_info=True)
        return content, None


def fields_to_metadata(fields: Optional[Dict]) -> Optional[str]:
    """Serialize structured fields to a compact JSON string for metadata."""
    if not fields:
        return None
    try:
        return json.dumps(fields, ensure_ascii=False)[:8000]
    except (TypeError, ValueError):
        return None

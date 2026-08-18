"""Tests for type-specific parsing strategies (§5.3)."""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from docreader.parser.pdf_type_strategies import (
    apply_contract_strategy,
    apply_financial_strategy,
    apply_resume_strategy,
    apply_academic_strategy,
    apply_type_strategy,
    extract_contract_fields,
    extract_financial_highlights,
    extract_resume_fields,
    fields_to_metadata,
)


class TestContractStrategy:
    def test_extracts_clauses_parties_dates(self):
        text = (
            "甲方：北京示例科技有限公司\n"
            "乙方：张三\n"
            "鉴于双方友好协商，特此达成如下协议：\n"
            "第一条 合同标的\n"
            "乙方为甲方提供软件开发服务。\n"
            "第二条 付款方式\n"
            "甲方应在2024年3月15日前支付首期款项。\n"
            "第三条 违约责任\n"
            "任何一方违约应承担赔偿责任。\n"
            "签署日期：2024年1月10日\n"
            "（签章）甲方代表签字：________\n"
        )
        fields = extract_contract_fields(text)
        assert fields["clause_count"] == 3
        roles = {p["role"] for p in fields["parties"]}
        assert any("甲方" in r for r in roles)
        assert fields["has_signature_block"] is True
        assert len(fields["key_dates"]) >= 1

    def test_clause_headings_promoted(self):
        text = "第一条 总则\n本合同自签署之日起生效。\n第二条 定义\n术语按本条解释。"
        enhanced, fields = apply_contract_strategy(text)
        assert "### 第一条" in enhanced
        assert "### 第二条" in enhanced

    def test_english_contract(self):
        text = (
            "Party A: ACME Corp\nParty B: John Doe\n"
            "Article 1. Scope\nThis Agreement covers licensing.\n"
            "Article 2. Payment\nDue by 2024-06-30.\n"
            "Effective Date: 2024-01-01\nSignature: ________\n"
        )
        fields = extract_contract_fields(text)
        assert fields["clause_count"] == 2
        assert any(p["name"] == "ACME Corp" for p in fields["parties"])


class TestFinancialStrategy:
    def test_extracts_metrics_and_yoy(self):
        text = (
            "2023年度报告\n"
            "营业收入 1,234,567.89 万元，同比增长 15.3%\n"
            "净利润 234,567 万元，同比下降 2.1%\n"
            "总资产 5,678,901 万元\n"
            "每股收益：1.25元\n"
        )
        fields = extract_financial_highlights(text)
        names = [m["metric"] for m in fields["key_metrics"]]
        assert "营业收入" in names
        assert "净利润" in names
        assert any(y["yoy_percent"] == "15.3" for y in fields["yoy_data"])

    def test_summary_prepended(self):
        text = "营业收入 100 万元\n正文内容" + "x" * 100
        enhanced, fields = apply_financial_strategy(text)
        assert enhanced.startswith("## 财务数据摘要")
        assert "营业收入: 100" in enhanced

    def test_precision_preserved(self):
        text = "净利润 1,234,567.89 万元"
        _, fields = apply_financial_strategy(text)
        m = fields["key_metrics"][0]
        assert m["value"] == "1,234,567.89"


class TestResumeStrategy:
    def test_extracts_structured_fields(self):
        text = (
            "李明\n"
            "男 | 本科 | 计算机科学\n"
            "教育背景\n2015.9-2019.6 北京大学 计算机科学与技术 本科\n"
            "工作经历\n2019.7-至今 腾讯科技有限公司 高级工程师\n"
            "项目经历\n企业级知识库检索系统\n"
            "专业技能\nPython, Go, Docker, Kubernetes, PostgreSQL\n"
        )
        fields = extract_resume_fields(text)
        assert fields["name"] == "李明"
        assert any("腾讯" in c["company"] for c in fields["companies"])
        assert "Python" in fields["skills"]
        assert any("知识库" in p for p in fields["projects"])
        assert any(e["degree"] == "本科" for e in fields["education"])

    def test_summary_card_prepended(self):
        text = "王芳\n技能\nJava, SQL\n工作经历\n2020.1-至今 阿里巴巴集团\n"
        enhanced, fields = apply_resume_strategy(text)
        assert "## 简历摘要" in enhanced
        assert "王芳" in enhanced


class TestAcademicStrategy:
    def test_citation_chain_preserved(self):
        text = (
            "Deep learning advances [1] have enabled progress. "
            "Transformers [2] improved NLP. Attention [3] is key.\n\n"
            "References\n"
            "[1] LeCun et al. Deep learning. Nature, 2015.\n"
            "[2] Vaswani et al. Attention is all you need. 2017.\n"
            "[3] Bahdanau et al. Neural machine translation. 2015.\n"
        )
        enhanced, fields = apply_academic_strategy(text)
        assert fields["citation_count"] == 3
        assert fields["has_references_section"] is True
        assert "## References" in enhanced
        # Reference entries intact after enhancement
        assert "[1] LeCun et al." in enhanced


class TestDispatcher:
    def test_general_returns_untouched(self):
        text = "普通文档内容" * 20
        result, fields = apply_type_strategy(text, "general")
        assert result == text
        assert fields is None

    def test_unknown_type_returns_untouched(self):
        text = "content"
        result, fields = apply_type_strategy(text, "nonexistent")
        assert result == text

    def test_never_raises(self):
        # None input must not raise
        result, _ = apply_type_strategy(None, "contract")  # type: ignore
        assert result is None

    def test_fields_to_metadata(self):
        assert fields_to_metadata(None) is None
        js = fields_to_metadata({"a": 1, "b": [1, 2]})
        assert json.loads(js) == {"a": 1, "b": [1, 2]}

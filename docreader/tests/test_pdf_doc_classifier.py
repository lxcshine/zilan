"""Tests for PDF document type classification (§5.3)."""

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from docreader.parser.pdf_doc_classifier import (
    classify_document,
    get_strategy_config,
    TYPE_ACADEMIC,
    TYPE_CONTRACT,
    TYPE_FINANCIAL,
    TYPE_RESUME,
    TYPE_MANUAL,
    TYPE_GENERAL,
)


class TestAcademicClassification:
    def test_arxiv_paper(self):
        text = """
        arXiv:2301.00001v1 [cs.CL] 5 Jan 2023
        Deep Learning for NLP: A Survey
        Abstract
        In this paper, we survey recent advances in deep learning for natural
        language processing. We discuss transformer architectures [1], attention
        mechanisms [2], and pre-trained language models [3].
        Keywords: deep learning, NLP, transformers
        1 Introduction
        The field of natural language processing has been revolutionized by
        deep learning approaches. As noted by Vaswani et al. [4], attention
        mechanisms have become central to modern NLP.
        Figure 1: Architecture of Transformer model.
        Table 1: Performance comparison on GLUE benchmark.
        References
        [1] Vaswani, A. et al. Attention is all you need. NeurIPS 2017.
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_ACADEMIC
        assert result.confidence > 0.5
        assert "dual_column" in result.strategy or result.strategy == "academic"

    def test_ieee_paper(self):
        text = """
        IEEE Transactions on Pattern Analysis and Machine Intelligence
        doi: 10.1109/TPAMI.2023.001
        A Novel Approach to Image Recognition
        Abstract—We propose a method for image recognition.
        Index Terms—Image recognition, deep learning, CNN.
        I. Introduction
        Theorem 1. Let f(x) be a continuous function.
        Proof. By induction on n.
        Figure 1: System overview.
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_ACADEMIC


class TestContractClassification:
    def test_chinese_contract(self):
        text = """
        合同编号：HT-2023-001
        甲方（出租人）：张三
        乙方（承租人）：李四
        鉴于甲方拥有合法产权的房屋，双方经协商达成如下协议：
        第一条 房屋基本情况
        甲方同意将位于北京市朝阳区的房屋出租给乙方。
        第二条 租赁期限
        本合同签订日期：2023年1月1日。租赁期限为一年。
        第三条 违约责任
        若乙方未按时支付租金，甲方有权解除本合同。
        本合同一式两份，甲乙双方各执一份，经盖章签字后生效。
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_CONTRACT
        assert result.strategy == "contract"

    def test_english_contract(self):
        text = """
        Contract No: SC-2023-456
        This Agreement is entered into by and between Party A and Party B.
        WHEREAS, Party A desires to engage Party B for services.
        NOW THEREFORE, the parties agree as follows:
        Article 1: Scope of Services
        Article 2: Payment Terms
        Article 3: Confidentiality Obligations
        Article 4: Intellectual Property
        Article 5: Force Majeure
        Date of Signing: March 15, 2023
        Signature: ___________
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_CONTRACT


class TestFinancialClassification:
    def test_chinese_financial_report(self):
        text = """
        营业收入：1,234,567万元
        净利润：234,567万元
        总资产：5,678,901万元
        资产负债表
        利润表
        现金流量表
        同比增长15.3%
        审计报告
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_FINANCIAL
        assert result.strategy == "financial"


class TestResumeClassification:
    def test_chinese_resume(self):
        text = """
        姓名：王五
        性别：男
        教育背景
        2018.9-2022.6  本科  计算机科学与技术
        工作经历
        2022.7-至今  某互联网公司  职位：后端开发工程师
        技能
        Python, Java, Go, Docker
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_RESUME


class TestManualClassification:
    def test_technical_manual(self):
        text = """
        Installation Guide
        Step 1: Download the package.
        Step 2: Configure settings in config.yaml.
        Warning: Do not skip Step 1.
        ```bash
        npm install -g weknora
        ```
        Troubleshooting
        If the service fails to start, check the diagnostic logs.
        HTTP API Reference
        The REST API accepts JSON payloads.
        1.1 Overview
        1.2 Configuration Parameters
        """
        result = classify_document(text)
        assert result.doc_type == TYPE_MANUAL


class TestGeneralClassification:
    def test_generic_text(self):
        text = "This is a general document about various topics. It does not fit any specific category."
        result = classify_document(text)
        assert result.doc_type == TYPE_GENERAL

    def test_short_text(self):
        result = classify_document("Hello")
        assert result.doc_type == TYPE_GENERAL
        assert result.confidence < 0.6


class TestStrategyConfig:
    def test_academic_strategy(self):
        config = get_strategy_config("academic")
        assert config["formula_extraction"] is True
        assert config["dual_column_enhanced"] is True
        assert config["preserve_citations"] is True

    def test_financial_strategy(self):
        config = get_strategy_config("financial")
        assert config["table_priority"] is True
        assert config["field_extraction"] is True

    def test_contract_strategy(self):
        config = get_strategy_config("contract")
        assert config["clause_extraction"] is True
        assert config["field_extraction"] is True

    def test_general_strategy(self):
        config = get_strategy_config("general")
        assert config["table_priority"] is True

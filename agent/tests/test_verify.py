from alertflow_agent.runtime.verify import build_catalog, fact_id, verify


def _evidence():
    return {"ana:status@0": {"kind": "analysis", "source_type": "analysis", "data": {"offline": {"n": 2}}, "summary": "2"}}


def _draft(catalog, **hyp):
    return {
        "summary": "two offline checks",
        "observations": [{
            "id": "o1",
            "text": "offline count",
            "evidence_ids": ["ana:status@0"],
            "fact_ids": [fact_id(catalog, "ana:status@0", "offline.n")],
            "facts": [{"evidence_id": "ana:status@0", "path": "invented", "value": 9}],
        }],
        "hypotheses": [{
            "id": "h1", "text": "probe", "layer": "probe", "status": "possible", "evidence_ids": ["ana:status@0"],
            **hyp,
        }],
        "unknowns": [],
        "recommended_next_steps": [],
    }


def test_catalog_facts_pass_and_ignore_model_values():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    result = verify(_draft(catalog), evidence, [], catalog=catalog)
    assert result["passed"]
    fact = result["draft"]["observations"][0]["facts"][0]
    assert fact == {"evidence_id": "ana:status@0", "path": "offline.n", "value": 2}
    assert result["draft"]["hypotheses"][0]["supporting_evidence_ids"] == ["ana:status@0"]


def test_missing_fact_or_evidence_fails():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    draft = _draft(catalog)
    draft["observations"][0]["fact_ids"] = ["no-such"]
    draft["observations"][0]["evidence_ids"] = ["missing-ev"]
    result = verify(draft, evidence, [], catalog=catalog)
    rules = {item["rule"] for item in result["findings"]}
    assert "fact_ref" in rules
    assert "evidence_ref" in rules
    assert result["draft"]["observations"][0]["facts"] == []


def test_possible_and_undetermined_do_not_need_evidence():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    for status in ("possible", "undetermined"):
        draft = _draft(catalog)
        draft["hypotheses"][0]["status"] = status
        draft["hypotheses"][0]["evidence_ids"] = []
        assert verify(draft, evidence, [], catalog=catalog)["passed"]


def test_supported_and_ruled_out_need_evidence():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    for status in ("supported", "ruled_out"):
        draft = _draft(catalog)
        draft["hypotheses"][0]["status"] = status
        draft["hypotheses"][0]["evidence_ids"] = []
        result = verify(draft, evidence, [], catalog=catalog)
        assert any(item["rule"] == "conclusion" for item in result["findings"])
    cited = _draft(catalog)
    cited["hypotheses"][0]["status"] = "ruled_out"
    passed = verify(cited, evidence, [], catalog=catalog)
    assert passed["passed"]
    assert passed["draft"]["hypotheses"][0]["weakening_evidence_ids"] == ["ana:status@0"]


def test_redaction_is_applied_without_a_finding():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    draft = _draft(catalog)
    draft["summary"] = "token secret-value stays out"
    result = verify(draft, evidence, ["secret-value"], catalog=catalog)
    assert result["passed"]
    assert "secret-value" not in result["draft"]["summary"]
    assert "[REDACTED]" in result["draft"]["summary"]

from alertflow_agent.runtime.context import build_context
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
            "id": "h1", "text": "probe", "layer": "probe", "status": "possible",
            "supporting_evidence_ids": ["ana:status@0"], "weakening_evidence_ids": [],
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
        draft["hypotheses"][0]["supporting_evidence_ids"] = []
        draft["hypotheses"][0]["weakening_evidence_ids"] = []
        assert verify(draft, evidence, [], catalog=catalog)["passed"]


def test_strong_conclusions_need_the_matching_direction():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    for status, field in (
        ("supported", "supporting_evidence_ids"),
        ("weakened", "weakening_evidence_ids"),
        ("ruled_out", "weakening_evidence_ids"),
    ):
        draft = _draft(catalog)
        draft["hypotheses"][0]["status"] = status
        draft["hypotheses"][0]["supporting_evidence_ids"] = []
        draft["hypotheses"][0]["weakening_evidence_ids"] = []
        result = verify(draft, evidence, [], catalog=catalog)
        assert any(item["rule"] == "conclusion" for item in result["findings"])
        draft["hypotheses"][0][field] = ["ana:status@0"]
        assert verify(draft, evidence, [], catalog=catalog)["passed"]


def test_one_hypothesis_keeps_both_directions():
    evidence = {
        "ana:status@0": {"kind": "analysis", "data": {"offline": {"n": 2}}},
        "evt:resolve": {"kind": "resolve", "data": {"duration_s": 30}},
    }
    catalog = build_catalog(evidence)
    draft = _draft(catalog)
    draft["hypotheses"][0]["status"] = "weakened"
    draft["hypotheses"][0]["supporting_evidence_ids"] = ["ana:status@0"]
    draft["hypotheses"][0]["weakening_evidence_ids"] = ["evt:resolve"]
    result = verify(draft, evidence, [], catalog=catalog)
    hyp = result["draft"]["hypotheses"][0]
    assert result["passed"]
    assert hyp["supporting_evidence_ids"] == ["ana:status@0"]
    assert hyp["weakening_evidence_ids"] == ["evt:resolve"]


def test_metric_series_is_not_a_fact_source():
    evidence = {
        "met:24h": {"kind": "metric_series", "data": {"series": [{"ts": 1, "avg_latency": 9}, {"ts": 2, "avg_latency": 11}]}},
        "ana:latency@0": {"kind": "analysis", "data": {"recent": {"median_ms": 40}, "median_ratio": 1.5}},
    }
    catalog = build_catalog(evidence)
    assert fact_id(catalog, "met:24h", "series.0.avg_latency") is None
    assert fact_id(catalog, "ana:latency@0", "recent.median_ms")
    prompt = build_context("write_report", {"event": {}, "evidence": evidence, "fact_catalog": catalog, "messages": []})
    assert "2 points from 1 to 2" in prompt
    assert "Latency statistics are in ana:latency" in prompt
    assert "{}" not in prompt.split("met:24h", 1)[1][:80]


def test_redaction_is_applied_without_a_finding():
    evidence = _evidence()
    catalog = build_catalog(evidence)
    draft = _draft(catalog)
    draft["summary"] = "token secret-value stays out"
    result = verify(draft, evidence, ["secret-value"], catalog=catalog)
    assert result["passed"]
    assert "secret-value" not in result["draft"]["summary"]
    assert "[REDACTED]" in result["draft"]["summary"]

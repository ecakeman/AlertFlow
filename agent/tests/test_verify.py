from alertflow_agent.runtime.verify import verify


def test_fact_must_match_evidence():
    evidence = {"ana:status@0": {"source_type": "analysis", "data": {"offline": {"n": 2}}, "summary": "2"}}
    draft = {
        "summary": "two offline checks",
        "observations": [{"id": "o1", "text": "offline count", "evidence_ids": ["ana:status@0"], "facts": [{"evidence_id": "ana:status@0", "path": "offline.n", "value": 2}]}],
        "hypotheses": [{
            "id": "h1", "text": "probe", "layer": "probe", "status": "possible", "confidence": "low", "rationale": "probe",
            "supporting_evidence_ids": ["ana:status@0"], "weakening_evidence_ids": [],
        }],
    }
    assert verify(draft, evidence, [])["passed"]
    draft["observations"][0]["facts"][0]["value"] = 9
    result = verify(draft, evidence, [])
    assert not result["passed"]
    assert result["findings"][0]["rule"] == "V3"
    draft["observations"][0]["facts"][0]["path"] = "missing.path"
    assert any(item["rule"] == "V3" for item in verify(draft, evidence, [])["findings"])
    tls = {
        "h1": {
            "id": "h1", "text": "tls", "layer": "tls", "status": "supported", "confidence": "low",
            "rationale": "errors", "supporting_evidence_ids": ["ana:errors@2"], "weakening_evidence_ids": [],
        }
    }
    evidence["ana:errors@0"] = {"source_type": "analysis", "data": {"classes": {"tls": 0}}}
    evidence["ana:errors@2"] = {"source_type": "analysis", "data": {"classes": {"tls": 3}}}
    later = dict(draft)
    later["observations"][0]["facts"][0]["path"] = "offline.n"
    later["observations"][0]["facts"][0]["value"] = 2
    later["hypotheses"] = [tls["h1"]]
    assert verify(later, evidence, [])["passed"]
    number_only = {
        "summary": "latency 20",
        "observations": [{"id": "o1", "text": "see hypothesis", "evidence_ids": [], "facts": []}],
        "hypotheses": [{
            "id": "h1", "text": "value 20", "layer": "probe", "status": "possible", "confidence": "low",
            "rationale": "metric", "supporting_evidence_ids": ["met:24h"], "weakening_evidence_ids": [],
        }],
    }
    metric = {"met:24h": {"source_type": "alertflow_metric", "summary": "20", "data": {"stats": {"last_avg_latency": 20}}}}
    warned = verify(number_only, metric, [])
    assert not any(item["detail"] == "20" for item in warned["warnings"])

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

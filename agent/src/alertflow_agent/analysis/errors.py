import re

_RULES = (
    ("timeout", re.compile(r"timeout|deadline exceeded", re.I)),
    ("connection_refused", re.compile(r"connection refused", re.I)),
    ("connection_reset", re.compile(r"reset|broken pipe|EOF")),
    ("dns", re.compile(r"no such host|lookup", re.I)),
    ("tls", re.compile(r"x509|tls|certificate|handshake", re.I)),
)


def classify_error(text: str, status_code: int | None = None) -> str:
    raw = text or ""
    for name, pattern in _RULES:
        if pattern.search(raw):
            return name
    if status_code is not None and status_code >= 400:
        return "http_status"
    if re.search(r"\b[45]\d\d\b", raw):
        return "http_status"
    if raw.strip():
        return "unknown"
    return "unknown"

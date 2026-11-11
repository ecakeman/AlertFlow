import { marked } from 'marked'
import DOMPurify from 'dompurify'

// 安全渲染 Markdown：marked 产出的 HTML 必须经过 DOMPurify 白名单过滤，
// 禁止原始 HTML 之外的危险标签/事件属性，避免 XSS。
marked.setOptions({
  gfm: true,
  breaks: true
})

const SANITIZE_OPTIONS = {
  USE_PROFILES: { html: true },
  FORBID_TAGS: ['script', 'style', 'iframe', 'object', 'embed', 'form', 'input', 'button', 'link', 'meta'],
  FORBID_ATTR: ['onerror', 'onload', 'onclick', 'onmouseover', 'onfocus', 'onblur', 'srcdoc', 'formaction', 'style']
}

export function renderMarkdown(source: string | null | undefined): string {
  if (!source) return ''
  const raw = marked.parse(source, { async: false })
  const html = typeof raw === 'string' ? raw : ''
  return DOMPurify.sanitize(html, SANITIZE_OPTIONS)
}

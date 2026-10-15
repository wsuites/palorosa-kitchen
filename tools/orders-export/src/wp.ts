/**
 * HTTP-only client for the store's WP All Export Pro. No browser: it logs into
 * wp-admin (cookie + nonce), rewrites the saved export's delivery-date filter,
 * then triggers and downloads the file through the export's cron key.
 */
import type { OrdersExportConfig } from './env'
import { readXlsxRows } from './xlsx'

const USER_AGENT =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36'

/** SiteGround's anti-bot challenge: HTTP 202 plus a JS captcha page, no login. */
function antiBotChallenge (response: Response, body: string): boolean {
  if (response.headers.get('sg-captcha')) return true
  return response.status === 202 && /sgcaptcha|robot challenge screen/i.test(body)
}

interface Field {
  name: string
  value: string
}

function decodeEntities (text: string): string {
  return text.replace(/&(#x?[0-9a-fA-F]+|amp|lt|gt|quot|apos);/g, (_all, entity: string) => {
    if (entity === 'amp') return '&'
    if (entity === 'lt') return '<'
    if (entity === 'gt') return '>'
    if (entity === 'quot') return '"'
    if (entity === 'apos') return "'"
    if (entity[0] === '#') {
      const code = entity[1] === 'x' || entity[1] === 'X' ? Number.parseInt(entity.slice(2), 16) : Number.parseInt(entity.slice(1), 10)
      return Number.isFinite(code) ? String.fromCodePoint(code) : ''
    }
    return ''
  })
}

function attr (tag: string, name: string): string | null {
  const match = new RegExp(`\\b${name}\\s*=\\s*"([^"]*)"`, 'i').exec(tag)
  return match ? decodeEntities(match[1] ?? '') : null
}

function extractForm (html: string): string | null {
  const forms = html.match(/<form\b[\s\S]*?<\/form>/gi) ?? []
  return forms.find((form) => form.includes('filter_rules_hierarhy')) ?? null
}

function parseFields (block: string): Field[] {
  const fields: Field[] = []
  for (const match of block.matchAll(/<input\b[^>]*>/gi)) {
    const tag = match[0]
    const name = attr(tag, 'name')
    const type = (attr(tag, 'type') ?? 'text').toLowerCase()
    if (!name || ['submit', 'button', 'file', 'image', 'reset'].includes(type)) continue
    if (type === 'checkbox' || type === 'radio') {
      if (/\bchecked\b/i.test(tag)) fields.push({ name, value: attr(tag, 'value') ?? 'on' })
    } else {
      fields.push({ name, value: attr(tag, 'value') ?? '' })
    }
  }
  for (const match of block.matchAll(/<select\b[^>]*name\s*=\s*"([^"]+)"[^>]*>([\s\S]*?)<\/select>/gi)) {
    const inner = match[2] ?? ''
    const selected =
      /<option\b[^>]*\bselected\b[^>]*\bvalue\s*=\s*"([^"]*)"/i.exec(inner)?.[1] ??
      /<option\b[^>]*\bvalue\s*=\s*"([^"]*)"[^>]*\bselected\b/i.exec(inner)?.[1] ??
      ''
    fields.push({ name: decodeEntities(match[1] ?? ''), value: decodeEntities(selected) })
  }
  for (const match of block.matchAll(/<textarea\b[^>]*name\s*=\s*"([^"]+)"[^>]*>([\s\S]*?)<\/textarea>/gi)) {
    fields.push({ name: decodeEntities(match[1] ?? ''), value: decodeEntities(match[2] ?? '') })
  }
  return fields
}

export class WpAllExportClient {
  private readonly cookies = new Map<string, string>()

  constructor (private readonly config: OrdersExportConfig) {}

  private cookieHeader (): string {
    return [...this.cookies].map(([name, value]) => `${name}=${value}`).join('; ')
  }

  private saveCookies (response: Response): void {
    const headers = response.headers as unknown as { getSetCookie?: () => string[] }
    const list = typeof headers.getSetCookie === 'function' ? headers.getSetCookie() : []
    for (const raw of list) {
      const pair = raw.split(';')[0] ?? ''
      const equals = pair.indexOf('=')
      if (equals > 0) this.cookies.set(pair.slice(0, equals).trim(), pair.slice(equals + 1).trim())
    }
  }

  async request (url: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers)
    headers.set('User-Agent', USER_AGENT)
    headers.set('Accept', 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8')
    headers.set('Accept-Language', 'es-ES,es;q=0.9')
    if (this.cookies.size > 0) headers.set('Cookie', this.cookieHeader())
    const response = await fetch(url, { ...init, headers })
    this.saveCookies(response)
    return response
  }

  async login (): Promise<void> {
    await (await this.request(`${this.config.base}/wp-login.php`)).text()
    const body = new URLSearchParams({
      log: this.config.user,
      pwd: this.config.password,
      'wp-submit': 'Acceder',
      redirect_to: `${this.config.base}/wp-admin/`,
      testcookie: '1',
    })
    const response = await this.request(`${this.config.base}/wp-login.php`, {
      method: 'POST',
      body,
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      redirect: 'manual',
    })
    const text = await response.text()
    if (![...this.cookies.keys()].some((name) => name.startsWith('wordpress_logged_in'))) {
      if (antiBotChallenge(response, text)) {
        throw new Error(
          'SiteGround anti-bot challenge (HTTP 202): the host is challenging this IP, so no session cookie comes back. ' +
            'Ask SiteGround to allowlist this machine in their Anti-Bot AI / SG-Captcha.'
        )
      }
      throw new Error('WP login failed (no session cookie). Check WP_ADMIN_USER / WP_ADMIN_PASSWORD.')
    }
  }

  private async optionsUrl (): Promise<string> {
    const manage = `${this.config.base}/wp-admin/admin.php?page=pmxe-admin-manage`
    const html = await (await this.request(manage)).text()
    const match =
      new RegExp(`id=${this.config.exportId}&#038;action=options&#038;_wpnonce_options=([a-f0-9]+)`).exec(html) ??
      new RegExp(`id=${this.config.exportId}&action=options&_wpnonce_options=([a-f0-9]+)`).exec(html)
    if (!match) throw new Error('WP All Export options nonce not found (wrong export id, or not an admin).')
    return `${this.config.base}/wp-admin/admin.php?page=pmxe-admin-manage&id=${this.config.exportId}&action=options&_wpnonce_options=${match[1]}`
  }

  private async loadOptions (): Promise<{ url: string; fields: Field[] }> {
    const url = await this.optionsUrl()
    const html = await (await this.request(url)).text()
    const form = extractForm(html)
    if (!form) throw new Error('WP All Export options form not found.')
    return { url, fields: parseFields(form) }
  }

  private setStoreDate (fields: Field[], storeDate: string): Field[] {
    return fields.map((field) => {
      if (field.name === 'wp_all_export_value[1]') return { ...field, value: storeDate }
      if (field.name === 'filter_rules_hierarhy') {
        try {
          const rules = JSON.parse(field.value) as Array<Record<string, unknown>>
          for (const rule of rules) if (rule.element === 'cf_Seleccionar una Fecha') rule.value = storeDate
          return { ...field, value: JSON.stringify(rules) }
        } catch {
          return field
        }
      }
      if (field.name === 'whereclause') {
        return { ...field, value: field.value.replace(/meta\.meta_value = '[^']*'/, `meta.meta_value = '${storeDate}'`) }
      }
      return field
    })
  }

  async setDeliveryDate (storeDate: string): Promise<void> {
    const { url, fields } = await this.loadOptions()
    const body = new URLSearchParams()
    for (const field of this.setStoreDate(fields, storeDate)) body.append(field.name, field.value)
    await (
      await this.request(url, {
        method: 'POST',
        body,
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      })
    ).text()
  }

  private async triggerAndProcess (): Promise<void> {
    const { base, exportId, cronKey } = this.config
    const query = `export_key=${encodeURIComponent(cronKey)}&export_id=${encodeURIComponent(exportId)}`
    await (await this.request(`${base}/wp-load.php?${query}&action=trigger`)).text()
    for (let attempt = 0; attempt < 15; attempt++) {
      const text = await (await this.request(`${base}/wp-load.php?${query}&action=processing`)).text()
      if (text.includes('complete')) return
    }
    throw new Error('WP All Export did not report completion.')
  }

  async download (): Promise<Uint8Array> {
    const { base, exportId, token } = this.config
    const url = `${base}/wp-load.php?security_token=${encodeURIComponent(token)}&export_id=${encodeURIComponent(exportId)}&action=get_data`
    const response = await this.request(url)
    if (!response.ok) throw new Error(`Export download failed: HTTP ${response.status}`)
    return new Uint8Array(await response.arrayBuffer())
  }

  /** Full run: login, set the delivery date, trigger and return the parsed rows. */
  async exportRows (storeDate: string): Promise<string[][]> {
    if (!this.config.user || !this.config.password) throw new Error('Missing WP_ADMIN_USER / WP_ADMIN_PASSWORD.')
    if (!this.config.cronKey) throw new Error('Missing WP_EXPORT_CRON_KEY.')
    if (!this.config.token) throw new Error('Missing WP_EXPORT_TOKEN (or WP_EXPORT_CRON_KEY to derive it).')
    await this.login()
    await this.setDeliveryDate(storeDate)
    await this.triggerAndProcess()
    return readXlsxRows(await this.download())
  }
}

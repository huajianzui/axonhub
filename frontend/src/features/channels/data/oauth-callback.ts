import { apiRequest } from '@/lib/api-client'

/**
 * Provider keys used by the loopback OAuth callback listener. They match the
 * backend's `oauthcallback` provider IDs.
 */
export type OAuthCallbackProvider = 'codex' | 'claudecode' | 'antigravity' | 'xai'

export interface OAuthCallbackProviderStatus {
  provider: OAuthCallbackProvider
  /** Whether AxonHub is listening on this provider's callback address. */
  active: boolean
  /** The address the provider redirects the browser to. */
  redirect_uri: string
}

export interface OAuthCallbackStatusResult {
  providers: OAuthCallbackProviderStatus[]
}

export interface OAuthCallbackPollResult {
  ready: boolean
  callback_url?: string
}

/**
 * Reports which loopback callback listeners AxonHub is holding.
 *
 * The console calls this when it opens an authorization flow: an active
 * listener means the callback will be captured automatically, so the paste
 * field can stay hidden. An inactive listener (the port is held by the
 * provider's CLI, or by another gateway) means the operator still has to paste
 * the callback URL.
 */
export async function oauthCallbackStatus(headers?: Record<string, string>): Promise<OAuthCallbackStatusResult> {
  return apiRequest('/admin/oauth/callback/status', {
    method: 'GET',
    headers,
    requireAuth: true,
  })
}

/**
 * Returns a callback URL captured on this provider's loopback listener.
 *
 * `ready: false` means nothing has been captured yet and the console should
 * keep polling. `session_id` is the OAuth state returned by the provider's
 * start endpoint; the backend refuses a capture that belongs to a different
 * session.
 */
export async function oauthCallbackPoll(
  provider: OAuthCallbackProvider,
  sessionId: string,
  headers?: Record<string, string>
): Promise<OAuthCallbackPollResult> {
  const query = new URLSearchParams({ provider, session_id: sessionId })

  return apiRequest(`/admin/oauth/callback/poll?${query.toString()}`, {
    method: 'GET',
    headers,
    requireAuth: true,
  })
}

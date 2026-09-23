import { useState, useCallback, useEffect, useRef } from 'react';
import { toast } from 'sonner';
import { useTranslation } from 'react-i18next';
import { ProxyType } from '../components/channels-proxy-dialog';
import {
  oauthCallbackPoll,
  oauthCallbackStatus,
  type OAuthCallbackProvider,
} from '../data/oauth-callback';

export interface ProxyConfig {
  type: ProxyType;
  url?: string;
  username?: string;
  password?: string;
}

export interface OAuthStartResult {
  session_id: string;
  auth_url: string;
}

export interface OAuthExchangeInput {
  session_id: string;
  callback_url: string;
  proxy?: ProxyConfig;
}

export interface OAuthExchangeResult {
  credentials: string;
}

export interface OAuthFlowOptions {
  /**
   * Function to call the OAuth start endpoint
   */
  startFn: (headers?: Record<string, string>) => Promise<OAuthStartResult>;

  /**
   * Function to call the OAuth exchange endpoint
   */
  exchangeFn: (input: OAuthExchangeInput, headers?: Record<string, string>) => Promise<OAuthExchangeResult>;

  /**
   * Optional proxy configuration for exchange token request
   */
  proxyConfig?: ProxyConfig;

  /**
   * Callback when credentials are successfully obtained
   */
  onSuccess?: (credentials: string) => void;

  /**
   * Provider key for the loopback callback listener. When set, the hook polls
   * the listener after starting authorization and completes the exchange
   * automatically, so the operator does not have to paste the callback URL.
   */
  callbackProvider?: OAuthCallbackProvider;
}

export interface OAuthFlowState {
  sessionId: string | null;
  authUrl: string | null;
  callbackUrl: string;
  isStarting: boolean;
  isExchanging: boolean;
  /**
   * True while waiting for the browser to reach the loopback callback
   * listener. The authorization URL has been opened and the console is polling.
   */
  isAwaitingCallback: boolean;
  /**
   * True when the listener captured the callback and the exchange is running
   * without operator input.
   */
  isAutoCompleting: boolean;
  /**
   * How the automatic capture ended. 'completed' means the credential was
   * imported and this flow is done; 'failed' means the authorization was
   * received but importing it failed, so the captured URL is left for a manual
   * retry. Null while no outcome has been reached.
   */
  autoCaptureOutcome: 'completed' | 'failed' | null;
  /**
   * The failure detail when auto capture failed, kept on screen so the operator
   * can see why instead of watching a toast disappear.
   */
  autoCaptureError: string | null;
  /**
   * True when AxonHub holds this provider's callback listener. When false the
   * operator must paste the callback URL.
   */
  isAutoCaptureAvailable: boolean;
}

export interface OAuthFlowActions {
  start: () => Promise<void>;
  exchange: () => Promise<void>;
  setCallbackUrl: (url: string) => void;
  reset: () => void;
}

/** How often the console asks the listener whether a callback arrived. */
const POLL_INTERVAL_MS = 1500;

/** How long the console keeps polling before giving up on auto capture. */
const POLL_TIMEOUT_MS = 5 * 60 * 1000;

/**
 * A reusable hook for managing OAuth flows (e.g., Codex, Claude Code).
 * This eliminates code duplication for different OAuth providers.
 *
 * When `callbackProvider` is set and AxonHub holds that provider's loopback
 * callback listener, authorization completes on its own: the hook polls for the
 * captured callback and exchanges it. Pasting the callback URL stays available
 * as a fallback.
 *
 * @example
 * ```tsx
 * const codexOAuth = useOAuthFlow({
 *   startFn: codexOAuthStart,
 *   exchangeFn: codexOAuthExchange,
 *   callbackProvider: 'codex',
 *   onSuccess: (credentials) => form.setValue('credentials.apiKey', credentials),
 * });
 * ```
 */
export function useOAuthFlow(options: OAuthFlowOptions): OAuthFlowState & OAuthFlowActions {
  const { startFn, exchangeFn, proxyConfig, onSuccess, callbackProvider } = options;
  const { t } = useTranslation();

  const [sessionId, setSessionId] = useState<string | null>(null);
  const [authUrl, setAuthUrl] = useState<string | null>(null);
  const [callbackUrl, setCallbackUrl] = useState('');
  const [isStarting, setIsStarting] = useState(false);
  const [isExchanging, setIsExchanging] = useState(false);
  const [isAwaitingCallback, setIsAwaitingCallback] = useState(false);
  const [isAutoCompleting, setIsAutoCompleting] = useState(false);
  const [isAutoCaptureAvailable, setIsAutoCaptureAvailable] = useState(false);
  const [autoCaptureOutcome, setAutoCaptureOutcome] = useState<'completed' | 'failed' | null>(null);
  const [autoCaptureError, setAutoCaptureError] = useState<string | null>(null);

  // A paste mid-flight must win over the poller: these refs let the polling
  // closure observe operator input and in-flight exchanges without re-running
  // the polling effect.
  const callbackUrlRef = useRef('');
  callbackUrlRef.current = callbackUrl;
  const exchangeInFlightRef = useRef(false);

  // The callbacks and options below are recreated on every render by callers
  // (usually inline arrow functions), so they must never appear in the polling
  // effect's dependencies: doing so tears the effect down and rebuilds it on
  // every render, which cancels an exchange that just succeeded and leaves the
  // panel stuck on "importing". They are read through a ref instead, which
  // always holds the latest value without changing the effect's identity.
  const latestRef = useRef({ exchangeFn, onSuccess, proxyConfig, t });
  latestRef.current = { exchangeFn, onSuccess, proxyConfig, t };

  // Builds the exchange payload, shared by the manual and captured paths.
  const buildExchangeInput = useCallback(
    (url: string, session: string): OAuthExchangeInput => {
      const input: OAuthExchangeInput = { session_id: session, callback_url: url };

      if (proxyConfig && proxyConfig.type === ProxyType.URL) {
        input.proxy = {
          type: proxyConfig.type,
          url: proxyConfig.url,
          ...(proxyConfig.username && { username: proxyConfig.username }),
          ...(proxyConfig.password && { password: proxyConfig.password }),
        };
      }

      return input;
    },
    [proxyConfig]
  );

  // Ask the backend whether this provider's loopback listener is active, so the
  // console can decide whether to poll or to show the paste field.
  useEffect(() => {
    if (!callbackProvider) {
      setIsAutoCaptureAvailable(false);
      return;
    }

    let cancelled = false;

    oauthCallbackStatus()
      .then((result) => {
        if (cancelled) {
          return;
        }

        const provider = result.providers?.find((item) => item.provider === callbackProvider);
        setIsAutoCaptureAvailable(Boolean(provider?.active));
      })
      .catch(() => {
        // A status failure only means we cannot know; fall back to pasting.
        if (!cancelled) {
          setIsAutoCaptureAvailable(false);
        }
      });

    return () => {
      cancelled = true;
    };
  }, [callbackProvider]);

  const exchange = useCallback(async () => {
    if (!sessionId) {
      toast.error(t('channels.dialogs.oauth.errors.sessionMissing'));
      return;
    }

    const url = callbackUrlRef.current.trim();
    if (!url) {
      toast.error(t('channels.dialogs.oauth.errors.callbackUrlRequired'));
      return;
    }

    setIsExchanging(true);
    try {
      const result = await exchangeFn(buildExchangeInput(url, sessionId));

      if (onSuccess) {
        onSuccess(result.credentials);
      }

      toast.success(t('channels.dialogs.oauth.messages.credentialsImported'));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      setIsExchanging(false);
    }
  }, [sessionId, exchangeFn, onSuccess, t, buildExchangeInput]);

  // Poll the loopback listener for a captured callback. When it arrives, fill
  // the field and run the same exchange the operator would have triggered.
  useEffect(() => {
    if (!callbackProvider || !isAutoCaptureAvailable || !sessionId || !isAwaitingCallback) {
      return;
    }

    // A paste mid-flight wins: stop competing with the operator.
    if (callbackUrlRef.current.trim()) {
      return;
    }

    let cancelled = false;
    const deadline = Date.now() + POLL_TIMEOUT_MS;

    const timer = window.setInterval(async () => {
      if (cancelled || exchangeInFlightRef.current) {
        return;
      }

      if (Date.now() > deadline) {
        window.clearInterval(timer);
        if (!cancelled) {
          setIsAwaitingCallback(false);
          toast.error(t('channels.dialogs.oauth.errors.autoCaptureTimeout'));
        }
        return;
      }

      try {
        const result = await oauthCallbackPoll(callbackProvider, sessionId);
        if (cancelled || !result.ready || !result.callback_url) {
          return;
        }

        exchangeInFlightRef.current = true;
        window.clearInterval(timer);
        setCallbackUrl(result.callback_url);
        setIsAutoCompleting(true);

        try {
          const exchanged = await latestRef.current.exchangeFn(
            buildExchangeInput(result.callback_url, sessionId)
          );
          if (!cancelled) {
            latestRef.current.onSuccess?.(exchanged.credentials);

            // Leave a terminal state on screen. Without it the pending banner
            // simply disappears and the paste field reappears holding the URL
            // that was already consumed, which reads as "nothing happened" even
            // though the credential was imported.
            setAutoCaptureOutcome('completed');
            setIsAwaitingCallback(false);
            toast.success(latestRef.current.t('channels.dialogs.oauth.autoCapture.completed'));
          }
        } catch (error) {
          if (!cancelled) {
            const message = error instanceof Error ? error.message : String(error);

            setAutoCaptureOutcome('failed');
            // Keep the failure visible in the panel, not only in a toast: the
            // toast disappears and leaves the operator staring at a stalled
            // "importing" state with no way to tell what went wrong.
            setAutoCaptureError(message);
            // Leave the captured URL in the field so the operator can retry by
            // hand instead of losing the authorization.
            toast.error(latestRef.current.t('channels.dialogs.oauth.errors.autoCaptureExchangeFailed', { message }));
          }
        } finally {
          exchangeInFlightRef.current = false;
          if (!cancelled) {
            setIsAutoCompleting(false);
          }
        }
      } catch {
        // A transient poll failure just means we try again on the next tick.
      }
    }, POLL_INTERVAL_MS);

    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
    // Deliberately excludes exchangeFn, onSuccess, proxyConfig and t: callers
    // pass inline functions, so depending on them would rebuild this effect on
    // every render, cancel a successful exchange and leave the panel stuck.
    // latestRef carries them instead.
  }, [
    callbackProvider,
    isAutoCaptureAvailable,
    sessionId,
    isAwaitingCallback,
    buildExchangeInput,
  ]);

  const start = useCallback(async () => {
    setIsStarting(true);
    try {
      const result = await startFn();
      setSessionId(result.session_id);
      setAuthUrl(result.auth_url);
      setCallbackUrl('');
      setAutoCaptureOutcome(null);
      setAutoCaptureError(null);

      if (callbackProvider && isAutoCaptureAvailable) {
        setIsAwaitingCallback(true);
        window.open(result.auth_url, '_blank', 'noopener,noreferrer');
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      setIsStarting(false);
    }
  }, [startFn, callbackProvider, isAutoCaptureAvailable]);

  const reset = useCallback(() => {
    setSessionId(null);
    setAuthUrl(null);
    setCallbackUrl('');
    setIsStarting(false);
    setIsExchanging(false);
    setIsAwaitingCallback(false);
    setIsAutoCompleting(false);
    setAutoCaptureOutcome(null);
    setAutoCaptureError(null);
  }, []);

  return {
    sessionId,
    authUrl,
    callbackUrl,
    isStarting,
    isExchanging,
    isAwaitingCallback,
    isAutoCompleting,
    isAutoCaptureAvailable,
    autoCaptureOutcome,
    autoCaptureError,
    start,
    exchange,
    setCallbackUrl,
    reset,
  };
}

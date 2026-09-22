package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/server/oauthcallback"
)

// OAuthCallbackHandlers exposes loopback-captured OAuth callbacks to the
// channel console.
//
// Subscription providers redirect the operator's browser to a fixed loopback
// address. AxonHub historically required the operator to copy that URL out of
// the browser and paste it back. The console now starts polling as soon as it
// opens the authorization URL, and submits whatever the callback listener
// captured to the provider's existing exchange endpoint. Pasting the URL by
// hand remains available as a fallback.
type OAuthCallbackHandlers struct {
	manager *oauthcallback.Manager
}

// OAuthCallbackHandlersParams resolves the callback manager.
type OAuthCallbackHandlersParams struct {
	fx.In

	Manager *oauthcallback.Manager
}

// NewOAuthCallbackHandlers builds the console-facing handler set.
func NewOAuthCallbackHandlers(params OAuthCallbackHandlersParams) *OAuthCallbackHandlers {
	return &OAuthCallbackHandlers{manager: params.Manager}
}

// RegisterRoutes mounts the callback capture routes on the admin group.
func (h *OAuthCallbackHandlers) RegisterRoutes(group *gin.RouterGroup) {
	group.GET("/oauth/callback/status", h.Status)
	group.GET("/oauth/callback/poll", h.Poll)
}

// OAuthCallbackProviderStatus describes one provider's callback listener.
type OAuthCallbackProviderStatus struct {
	// Provider is the console provider key.
	Provider string `json:"provider"`

	// Active reports whether the loopback listener is bound. When false the
	// console must fall back to pasting the callback URL by hand.
	Active bool `json:"active"`

	// RedirectURI is the address the provider redirects the browser to.
	RedirectURI string `json:"redirect_uri"`
}

// StatusOAuthCallbackResponse reports which callback listeners are available.
type StatusOAuthCallbackResponse struct {
	Providers []OAuthCallbackProviderStatus `json:"providers"`
}

// Status reports the availability of every configured callback listener.
//
// The console calls this when it opens the channel dialog to decide whether to
// poll for a callback or show the paste field immediately.
//
// GET /admin/oauth/callback/status.
func (h *OAuthCallbackHandlers) Status(c *gin.Context) {
	providers := h.manager.ProviderStatuses()

	out := make([]OAuthCallbackProviderStatus, 0, len(providers))
	for _, p := range providers {
		out = append(out, OAuthCallbackProviderStatus{
			Provider:    p.Provider,
			Active:      p.Active,
			RedirectURI: p.RedirectURI,
		})
	}

	c.JSON(http.StatusOK, StatusOAuthCallbackResponse{Providers: out})
}

// PollOAuthCallbackResponse carries a captured callback URL.
type PollOAuthCallbackResponse struct {
	// Ready is false while no callback has been observed yet.
	Ready bool `json:"ready"`

	// CallbackURL is the URL the provider redirected the browser to. Submit it
	// to the provider's existing exchange endpoint to finish authorization.
	CallbackURL string `json:"callback_url,omitempty"`
}

// Poll returns a callback captured for one provider, if any.
//
// The console polls this endpoint right after it opens the authorization URL.
// A callback appears once the operator approves access in the browser.
//
// GET /admin/oauth/callback/poll?provider=codex&session_id=<oauth state>.
func (h *OAuthCallbackHandlers) Poll(c *gin.Context) {
	provider := c.Query("provider")
	if provider == "" {
		JSONError(c, http.StatusBadRequest, errors.New("provider is required"))

		return
	}

	callbackURL, ok := h.manager.Poll(provider, c.Query("session_id"))
	if !ok {
		c.JSON(http.StatusOK, PollOAuthCallbackResponse{Ready: false})

		return
	}

	c.JSON(http.StatusOK, PollOAuthCallbackResponse{Ready: true, CallbackURL: callbackURL})
}

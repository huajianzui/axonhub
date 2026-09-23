package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
)

// ChannelAccountHandlers exposes the subscription accounts of a channel.
//
// Accounts are read through GraphQL, which is what the console lists them with.
// The write paths live here instead, because adding an account takes the opaque
// credential string a provider's OAuth exchange returns, and that is a REST
// shape rather than a GraphQL input.
type ChannelAccountHandlers struct {
	accountService *biz.ChannelAccountService
	channelService *biz.ChannelService
}

// ChannelAccountHandlersParams resolves the services the handler needs.
type ChannelAccountHandlersParams struct {
	fx.In

	AccountService *biz.ChannelAccountService
	ChannelService *biz.ChannelService
}

// NewChannelAccountHandlers builds the account handler set.
func NewChannelAccountHandlers(params ChannelAccountHandlersParams) *ChannelAccountHandlers {
	return &ChannelAccountHandlers{
		accountService: params.AccountService,
		channelService: params.ChannelService,
	}
}

// RegisterRoutes mounts the account write routes on the admin group.
func (h *ChannelAccountHandlers) RegisterRoutes(group *gin.RouterGroup) {
	group.POST("/channels/:channel_id/accounts", h.Create)
	group.POST("/channels/:channel_id/accounts/:account_id/enable", h.SetEnabled(true))
	group.POST("/channels/:channel_id/accounts/:account_id/disable", h.SetEnabled(false))
	group.DELETE("/channels/:channel_id/accounts/:account_id", h.Delete)
}

// CreateChannelAccountRequest carries a grant obtained from a provider's OAuth
// exchange.
type CreateChannelAccountRequest struct {
	// Credentials is the opaque string the exchange returned, exactly as the
	// console received it.
	Credentials string `json:"credentials" binding:"required"`

	// Name is an optional operator label, typically the account email.
	Name string `json:"name,omitempty"`
}

// CreateChannelAccountResponse reports the account that was created or matched.
type CreateChannelAccountResponse struct {
	ID        int    `json:"id"`
	ChannelID int    `json:"channel_id"`
	Name      string `json:"name"`
	AuthState string `json:"auth_state"`
	Enabled   bool   `json:"enabled"`
	Weight    int    `json:"weight"`
}

// Create adds an account to a channel.
//
// POST /admin/channels/:channel_id/accounts.
func (h *ChannelAccountHandlers) Create(c *gin.Context) {
	ctx := c.Request.Context()

	channelID, err := parsePositiveInt(c.Param("channel_id"))
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)

		return
	}

	var req CreateChannelAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))

		return
	}

	account, err := h.accountService.CreateAccountFromCredentials(ctx, channelID, req.Credentials)
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)

		return
	}

	c.JSON(http.StatusOK, ChannelAccountResponseFrom(account))
}

// SetEnabled returns a handler that enables or disables one account.
//
// Disabling is an operator action and is independent of the account's
// authorization state: a paused account keeps its grant and its history.
func (h *ChannelAccountHandlers) SetEnabled(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		channelID, err := parsePositiveInt(c.Param("channel_id"))
		if err != nil {
			JSONError(c, http.StatusBadRequest, err)

			return
		}

		accountID, err := parsePositiveInt(c.Param("account_id"))
		if err != nil {
			JSONError(c, http.StatusBadRequest, err)

			return
		}

		if err := h.accountService.SetAccountEnabled(ctx, channelID, accountID, enabled); err != nil {
			JSONError(c, http.StatusBadRequest, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"enabled": enabled})
	}
}

// Delete removes an account from a channel.
//
// The row is soft-deleted so its history survives, and the channel is touched so
// the runtime stops selecting it.
//
// DELETE /admin/channels/:channel_id/accounts/:account_id.
func (h *ChannelAccountHandlers) Delete(c *gin.Context) {
	ctx := c.Request.Context()

	channelID, err := parsePositiveInt(c.Param("channel_id"))
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)

		return
	}

	accountID, err := parsePositiveInt(c.Param("account_id"))
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)

		return
	}

	if err := h.accountService.DeleteAccount(ctx, channelID, accountID); err != nil {
		JSONError(c, http.StatusBadRequest, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ChannelAccountResponse is the operator-facing view of an account. It never
// carries credential material.
type ChannelAccountResponse struct {
	ID        int    `json:"id"`
	ChannelID int    `json:"channel_id"`
	Name      string `json:"name"`
	Identity  string `json:"identity"`
	AuthState string `json:"auth_state"`
	Enabled   bool   `json:"enabled"`
	Weight    int    `json:"weight"`
}

// ChannelAccountResponseFrom projects an account onto its response shape.
func ChannelAccountResponseFrom(account *ent.ChannelAccount) ChannelAccountResponse {
	return ChannelAccountResponse{
		ID:        account.ID,
		ChannelID: account.ChannelID,
		Name:      account.Name,
		Identity:  account.Identity,
		AuthState: account.AuthState.String(),
		Enabled:   account.Enabled,
		Weight:    account.Weight,
	}
}

// parsePositiveInt parses a path parameter that must be a positive integer.
func parsePositiveInt(raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, errors.New("invalid id")
	}

	return value, nil
}

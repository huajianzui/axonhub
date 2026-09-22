package oauthcallback

import (
	"go.uber.org/fx"
)

// Module provides the loopback OAuth callback manager. The console-facing
// handler lives in the api package, which consumes the manager.
var Module = fx.Module("oauthcallback",
	fx.Provide(BuiltinProviders),
	fx.Provide(NewManager),
)

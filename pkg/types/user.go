package types

// UserRole represents the role of a user in the MCPJungle system.
type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

// User represents an authenticated, human user in mcpjungle
// A user has lesser privileges than an Admin.
// They can consume mcpjungle but not necessarily manage it.
type User struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type CreateOrUpdateUserRequest struct {
	Username    string `json:"username"`
	AccessToken string `json:"access_token,omitempty"`
}

type CreateOrUpdateUserResponse struct {
	Username    string `json:"username"`
	Role        string `json:"role"`
	AccessToken string `json:"access_token"`
}

// UserConfig describes the JSON configuration for creating a user.
type UserConfig struct {
	Username       string         `json:"username"`
	AccessToken    string         `json:"access_token"`
	AccessTokenEnv string         `json:"access_token.env"`
	AccessTokenRef AccessTokenRef `json:"access_token_ref"`
}

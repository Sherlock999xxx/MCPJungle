package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/internal/service/mcp"
	"github.com/mcpjungle/mcpjungle/internal/service/toolgroup"
	"github.com/mcpjungle/mcpjungle/pkg/types"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	entityMcpServer = "mcp_server"
	entityMcpClient = "mcp_client"
	entityGroup     = "group"
	entityUser      = "user"
)

// Options configures config directory synchronization.
type Options struct {
	Enabled bool
	Dir     string
}

type Services struct {
	DB               *gorm.DB
	MCPService       *mcp.MCPService
	ToolGroupService *toolgroup.ToolGroupService
}

type Service struct {
	opts     Options
	services Services

	resolvedDir string

	watcher *fsnotify.Watcher
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(opts Options, services Services) (*Service, error) {
	if services.DB == nil || services.MCPService == nil || services.ToolGroupService == nil {
		return nil, fmt.Errorf("config sync requires DB, MCP service and ToolGroup service")
	}
	if !opts.Enabled {
		return &Service{opts: opts, services: services}, nil
	}
	resolved, err := resolveConfigDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	return &Service{opts: opts, services: services, resolvedDir: resolved}, nil
}

func resolveConfigDir(dir string) (string, error) {
	target := strings.TrimSpace(dir)
	if target == "" {
		target = "~/.mcpjungle"
	}
	if strings.HasPrefix(target, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if target == "~" {
			target = home
		} else if strings.HasPrefix(target, "~/") {
			target = filepath.Join(home, target[2:])
		}
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func (s *Service) Start(ctx context.Context) error {
	if !s.opts.Enabled {
		return nil
	}
	if err := ensureSubDirs(s.resolvedDir); err != nil {
		return err
	}
	if err := s.Reconcile(ctx); err != nil {
		return fmt.Errorf("initial config reconciliation failed: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	s.watcher = watcher
	for _, d := range []string{
		s.resolvedDir,
		filepath.Join(s.resolvedDir, "mcp_servers"),
		filepath.Join(s.resolvedDir, "mcp_clients"),
		filepath.Join(s.resolvedDir, "groups"),
		filepath.Join(s.resolvedDir, "users"),
	} {
		if err := watcher.Add(d); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("failed to watch %s: %w", d, err)
		}
	}

	watchCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go s.watchLoop(watchCtx)
	return nil
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.watcher != nil {
		_ = s.watcher.Close()
	}
}

func (s *Service) watchLoop(ctx context.Context) {
	defer s.wg.Done()
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[config-sync] watcher error: %v", err)
		case ev, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if !isRelevantEvent(ev) {
				continue
			}
			pending = true
			debounce.Reset(300 * time.Millisecond)
		case <-debounce.C:
			if pending {
				if err := s.Reconcile(context.Background()); err != nil {
					log.Printf("[config-sync] reconcile failed after file changes: %v", err)
				}
				pending = false
			}
		}
	}
}

func isRelevantEvent(ev fsnotify.Event) bool {
	return ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0
}

func ensureSubDirs(root string) error {
	for _, sub := range []string{"mcp_servers", "mcp_clients", "groups", "users"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return fmt.Errorf("failed to create config subdirectory %s: %w", sub, err)
		}
	}
	return nil
}

func (s *Service) Reconcile(ctx context.Context) error {
	var errs []error
	if err := s.reconcileMcpServers(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.reconcileMcpClients(); err != nil {
		errs = append(errs, err)
	}
	if err := s.reconcileGroups(); err != nil {
		errs = append(errs, err)
	}
	if err := s.reconcileUsers(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		for _, err := range errs {
			log.Printf("[config-sync] %v", err)
		}
		return errors.Join(errs...)
	}
	return nil
}

type desiredFile[T any] struct {
	entity   T
	name     string
	path     string
	hash     string
	parseErr error
}

func loadDesired[T any](dir string, nameFn func(T) string) (map[string]desiredFile[T], map[string]bool, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, []error{fmt.Errorf("failed to read directory %s: %w", dir, err)}
	}
	result := map[string]desiredFile[T]{}
	blocked := map[string]bool{}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to read config file %s: %w", full, err))
			blocked[full] = true
			continue
		}
		var conf T
		if err := json.Unmarshal(raw, &conf); err != nil {
			errs = append(errs, fmt.Errorf("invalid JSON in %s: %w", full, err))
			blocked[full] = true
			continue
		}
		name := strings.TrimSpace(nameFn(conf))
		if name == "" {
			errs = append(errs, fmt.Errorf("config file %s does not define a valid name", full))
			blocked[full] = true
			continue
		}
		if existing, ok := result[name]; ok {
			errs = append(errs, fmt.Errorf("conflict: duplicate %s defined by %s and %s", name, existing.path, full))
			blocked[full] = true
			blocked[existing.path] = true
			continue
		}
		h := sha256.Sum256(raw)
		result[name] = desiredFile[T]{
			entity: conf,
			name:   name,
			path:   full,
			hash:   hex.EncodeToString(h[:]),
		}
	}
	return result, blocked, errs
}

func (s *Service) loadManaged(entityType string) (map[string]model.ManagedConfigFile, error) {
	var rows []model.ManagedConfigFile
	if err := s.services.DB.Where("entity_type = ?", entityType).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]model.ManagedConfigFile, len(rows))
	for _, r := range rows {
		out[r.EntityName] = r
	}
	return out, nil
}

func toJSON(v any) (datatypes.JSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

func (s *Service) createOrUpdateManagedRow(entityType, name, path, hash string) error {
	var row model.ManagedConfigFile
	err := s.services.DB.Where("entity_type = ? AND entity_name = ?", entityType, name).First(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.services.DB.Create(&model.ManagedConfigFile{
			EntityType: entityType,
			EntityName: name,
			FilePath:   path,
			FileHash:   hash,
		}).Error
	}
	row.FilePath = path
	row.FileHash = hash
	return s.services.DB.Save(&row).Error
}

func (s *Service) deleteManagedRow(entityType, name string) error {
	return s.services.DB.Unscoped().Where("entity_type = ? AND entity_name = ?", entityType, name).Delete(&model.ManagedConfigFile{}).Error
}

func (s *Service) reconcileMcpServers(ctx context.Context) error {
	desired, blocked, parseErrs := loadDesired[types.RegisterServerInput](filepath.Join(s.resolvedDir, "mcp_servers"), func(i types.RegisterServerInput) string { return i.Name })
	managed, err := s.loadManaged(entityMcpServer)
	if err != nil {
		return fmt.Errorf("failed to load tracked mcp server configs: %w", err)
	}
	var errs []error
	errs = append(errs, parseErrs...)

	for name, d := range desired {
		transport, err := types.ValidateTransport(d.entity.Transport)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid transport in %s: %w", d.path, err))
			continue
		}
		sessionMode, err := types.ValidateSessionMode(d.entity.SessionMode)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid session_mode in %s: %w", d.path, err))
			continue
		}
		server, err := newServerFromInput(d.entity, transport, sessionMode)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid mcp server config in %s: %w", d.path, err))
			continue
		}

		existing, getErr := s.services.MCPService.GetMcpServer(name)
		if getErr != nil && !errors.Is(getErr, gorm.ErrRecordNotFound) {
			errs = append(errs, fmt.Errorf("failed to read mcp server %s: %w", name, getErr))
			continue
		}

		track, tracked := managed[name]
		if errors.Is(getErr, gorm.ErrRecordNotFound) {
			if err := s.services.MCPService.RegisterMcpServer(ctx, server); err != nil {
				errs = append(errs, fmt.Errorf("failed to create mcp server %s from %s: %w", name, d.path, err))
				continue
			}
			if err := s.createOrUpdateManagedRow(entityMcpServer, name, d.path, d.hash); err != nil {
				errs = append(errs, fmt.Errorf("failed to track mcp server %s: %w", name, err))
			}
			continue
		}

		if !tracked {
			// adopt existing manually managed entity
			tracked = true
			track = model.ManagedConfigFile{EntityName: name}
		}

		if tracked && track.FileHash == d.hash {
			continue
		}

		if !serverEqual(existing, server) {
			if err := s.services.MCPService.DeregisterMcpServer(name); err != nil {
				errs = append(errs, fmt.Errorf("failed to deregister mcp server %s for update: %w", name, err))
				continue
			}
			if err := s.services.MCPService.RegisterMcpServer(ctx, server); err != nil {
				errs = append(errs, fmt.Errorf("failed to register updated mcp server %s from %s: %w", name, d.path, err))
				continue
			}
		}
		if err := s.createOrUpdateManagedRow(entityMcpServer, name, d.path, d.hash); err != nil {
			errs = append(errs, fmt.Errorf("failed to track mcp server %s: %w", name, err))
		}
	}

	for name, trackedRow := range managed {
		if _, ok := desired[name]; ok {
			continue
		}
		if blocked[trackedRow.FilePath] || fileExists(trackedRow.FilePath) {
			continue
		}
		if err := s.services.MCPService.DeregisterMcpServer(name); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete managed mcp server %s after file removal: %w", name, err))
			continue
		}
		if err := s.deleteManagedRow(entityMcpServer, name); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove tracking for mcp server %s: %w", name, err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func newServerFromInput(input types.RegisterServerInput, transport types.McpServerTransport, sessionMode types.SessionMode) (*model.McpServer, error) {
	switch transport {
	case types.TransportStreamableHTTP:
		return model.NewStreamableHTTPServer(input.Name, input.Description, input.URL, input.BearerToken, input.Headers, sessionMode)
	case types.TransportStdio:
		return model.NewStdioServer(input.Name, input.Description, input.Command, input.Args, input.Env, sessionMode)
	default:
		return model.NewSSEServer(input.Name, input.Description, input.URL, input.BearerToken, sessionMode)
	}
}

func serverEqual(a, b *model.McpServer) bool {
	return a.Name == b.Name && a.Description == b.Description && a.Transport == b.Transport && a.SessionMode == b.SessionMode && slices.Equal(a.Config, b.Config)
}

func (s *Service) reconcileMcpClients() error {
	desired, blocked, parseErrs := loadDesired[types.McpClientConfig](filepath.Join(s.resolvedDir, "mcp_clients"), func(i types.McpClientConfig) string { return i.Name })
	managed, err := s.loadManaged(entityMcpClient)
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, parseErrs...)

	for name, d := range desired {
		allowJSON, err := toJSON(d.entity.AllowMcpServers)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid allow list in %s: %w", d.path, err))
			continue
		}
		accessToken, err := resolveAccessTokenFromConfig(d.entity.AccessToken, d.entity.AccessTokenRef)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to resolve access token in %s: %w", d.path, err))
			continue
		}
		if accessToken == "" {
			errs = append(errs, fmt.Errorf("mcp client config %s must provide access token or access_token_ref", d.path))
			continue
		}

		var existing model.McpClient
		err = s.services.DB.Where("name = ?", name).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			errs = append(errs, fmt.Errorf("failed to fetch mcp client %s: %w", name, err))
			continue
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := s.services.DB.Create(&model.McpClient{
				Name:        name,
				Description: d.entity.Description,
				AccessToken: accessToken,
				AllowList:   allowJSON,
			}).Error; err != nil {
				errs = append(errs, fmt.Errorf("failed to create mcp client %s: %w", name, err))
				continue
			}
		} else {
			if existing.Description != d.entity.Description || existing.AccessToken != accessToken || !slices.Equal(existing.AllowList, allowJSON) {
				existing.Description = d.entity.Description
				existing.AccessToken = accessToken
				existing.AllowList = allowJSON
				if err := s.services.DB.Save(&existing).Error; err != nil {
					errs = append(errs, fmt.Errorf("failed to update mcp client %s: %w", name, err))
					continue
				}
			}
		}
		if err := s.createOrUpdateManagedRow(entityMcpClient, name, d.path, d.hash); err != nil {
			errs = append(errs, fmt.Errorf("failed to track mcp client %s: %w", name, err))
		}
	}

	for name, trackedRow := range managed {
		if _, ok := desired[name]; ok {
			continue
		}
		if blocked[trackedRow.FilePath] || fileExists(trackedRow.FilePath) {
			continue
		}
		if err := s.services.DB.Unscoped().Where("name = ?", name).Delete(&model.McpClient{}).Error; err != nil {
			errs = append(errs, fmt.Errorf("failed to delete managed mcp client %s after file removal: %w", name, err))
			continue
		}
		if err := s.deleteManagedRow(entityMcpClient, name); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove tracking for mcp client %s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (s *Service) reconcileGroups() error {
	desired, blocked, parseErrs := loadDesired[types.ToolGroup](filepath.Join(s.resolvedDir, "groups"), func(i types.ToolGroup) string { return i.Name })
	managed, err := s.loadManaged(entityGroup)
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, parseErrs...)

	for name, d := range desired {
		incTools, err := toJSON(d.entity.IncludedTools)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid included_tools in %s: %w", d.path, err))
			continue
		}
		incServers, err := toJSON(d.entity.IncludedServers)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid included_servers in %s: %w", d.path, err))
			continue
		}
		exclTools, err := toJSON(d.entity.ExcludedTools)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid excluded_tools in %s: %w", d.path, err))
			continue
		}
		groupModel := &model.ToolGroup{Name: name, Description: d.entity.Description, IncludedTools: incTools, IncludedServers: incServers, ExcludedTools: exclTools}
		_, tracked := managed[name]
		old, err := s.services.ToolGroupService.GetToolGroup(name)
		if err != nil {
			if errors.Is(err, toolgroup.ErrToolGroupNotFound) {
				if err := s.services.ToolGroupService.CreateToolGroup(groupModel); err != nil {
					errs = append(errs, fmt.Errorf("failed to create group %s from %s: %w", name, d.path, err))
					continue
				}
			} else {
				errs = append(errs, fmt.Errorf("failed to fetch group %s: %w", name, err))
				continue
			}
		} else {
			if !tracked || !groupEqual(old, groupModel) {
				if _, err := s.services.ToolGroupService.UpdateToolGroup(name, groupModel); err != nil {
					errs = append(errs, fmt.Errorf("failed to update group %s from %s: %w", name, d.path, err))
					continue
				}
			}
		}
		if err := s.createOrUpdateManagedRow(entityGroup, name, d.path, d.hash); err != nil {
			errs = append(errs, fmt.Errorf("failed to track group %s: %w", name, err))
		}
	}
	for name, trackedRow := range managed {
		if _, ok := desired[name]; ok {
			continue
		}
		if blocked[trackedRow.FilePath] || fileExists(trackedRow.FilePath) {
			continue
		}
		if err := s.services.ToolGroupService.DeleteToolGroup(name); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete managed group %s after file removal: %w", name, err))
			continue
		}
		if err := s.deleteManagedRow(entityGroup, name); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove tracking for group %s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func groupEqual(a, b *model.ToolGroup) bool {
	return a.Name == b.Name && a.Description == b.Description && slices.Equal(a.IncludedTools, b.IncludedTools) && slices.Equal(a.IncludedServers, b.IncludedServers) && slices.Equal(a.ExcludedTools, b.ExcludedTools)
}

func (s *Service) reconcileUsers() error {
	desired, blocked, parseErrs := loadDesired[types.UserConfig](filepath.Join(s.resolvedDir, "users"), func(i types.UserConfig) string { return i.Username })
	managed, err := s.loadManaged(entityUser)
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, parseErrs...)

	for name, d := range desired {
		accessToken, err := resolveAccessTokenFromConfig(d.entity.AccessToken, d.entity.AccessTokenRef)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to resolve access token in %s: %w", d.path, err))
			continue
		}
		if accessToken == "" {
			errs = append(errs, fmt.Errorf("user config %s must provide access token or access_token_ref", d.path))
			continue
		}

		var existing model.User
		err = s.services.DB.Where("username = ?", name).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			errs = append(errs, fmt.Errorf("failed to fetch user %s: %w", name, err))
			continue
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := s.services.DB.Create(&model.User{Username: name, Role: types.UserRoleUser, AccessToken: accessToken}).Error; err != nil {
				errs = append(errs, fmt.Errorf("failed to create user %s: %w", name, err))
				continue
			}
		} else {
			if existing.Role == types.UserRoleAdmin {
				errs = append(errs, fmt.Errorf("config sync cannot manage admin user %s (file: %s)", name, d.path))
				continue
			}
			if existing.AccessToken != accessToken {
				existing.AccessToken = accessToken
				if err := s.services.DB.Save(&existing).Error; err != nil {
					errs = append(errs, fmt.Errorf("failed to update user %s: %w", name, err))
					continue
				}
			}
		}

		if err := s.createOrUpdateManagedRow(entityUser, name, d.path, d.hash); err != nil {
			errs = append(errs, fmt.Errorf("failed to track user %s: %w", name, err))
		}
	}
	for name, trackedRow := range managed {
		if _, ok := desired[name]; ok {
			continue
		}
		if blocked[trackedRow.FilePath] || fileExists(trackedRow.FilePath) {
			continue
		}
		var user model.User
		if err := s.services.DB.Where("username = ?", name).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				_ = s.deleteManagedRow(entityUser, name)
				continue
			}
			errs = append(errs, fmt.Errorf("failed to fetch managed user %s during deletion: %w", name, err))
			continue
		}
		if user.Role == types.UserRoleAdmin {
			errs = append(errs, fmt.Errorf("config sync cannot delete admin user %s", name))
			continue
		}
		if err := s.services.DB.Unscoped().Where("username = ?", name).Delete(&model.User{}).Error; err != nil {
			errs = append(errs, fmt.Errorf("failed to delete managed user %s after file removal: %w", name, err))
			continue
		}
		if err := s.deleteManagedRow(entityUser, name); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove tracking for user %s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func resolveAccessTokenFromConfig(accessToken string, ref types.AccessTokenRef) (string, error) {
	if strings.TrimSpace(accessToken) != "" {
		return strings.TrimSpace(accessToken), nil
	}
	if strings.TrimSpace(ref.Env) != "" {
		v, ok := os.LookupEnv(ref.Env)
		if ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return v, nil
			}
		}
		if strings.TrimSpace(ref.File) == "" {
			return "", fmt.Errorf("environment variable %s is not set or empty", ref.Env)
		}
	}
	if strings.TrimSpace(ref.File) != "" {
		data, err := os.ReadFile(ref.File)
		if err != nil {
			return "", fmt.Errorf("failed to read access token file %s: %w", ref.File, err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("access token file %s is empty", ref.File)
		}
		return v, nil
	}
	return "", nil
}

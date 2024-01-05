// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package vault

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/helper/versions"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/hashicorp/vault/sdk/helper/consts"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/sdk/plugin"
)

const (
	pluginReloadPluginsType = "plugins"
	pluginReloadMountsType  = "mounts"
)

// reloadMatchingPluginMounts reloads provided mounts, regardless of
// plugin name, as long as the backend type is plugin.
func (c *Core) reloadMatchingPluginMounts(ctx context.Context, ns *namespace.Namespace, mounts []string) error {
	c.mountsLock.RLock()
	defer c.mountsLock.RUnlock()
	c.authLock.RLock()
	defer c.authLock.RUnlock()

	var errors error
	for _, mount := range mounts {
		var isAuth bool
		// allow any of
		//   - sys/auth/foo/
		//   - sys/auth/foo
		//   - auth/foo/
		//   - auth/foo
		if strings.HasPrefix(mount, credentialRoutePrefix) {
			isAuth = true
		} else if strings.HasPrefix(mount, mountPathSystem+credentialRoutePrefix) {
			isAuth = true
			mount = strings.TrimPrefix(mount, mountPathSystem)
		}
		if !strings.HasSuffix(mount, "/") {
			mount += "/"
		}

		entry := c.router.MatchingMountEntry(ctx, mount)
		if entry == nil {
			errors = multierror.Append(errors, fmt.Errorf("cannot fetch mount entry on %q", mount))
			continue
		}

		// We dont reload mounts that are not in the same namespace
		if ns.ID != entry.Namespace().ID {
			continue
		}

		err := c.reloadBackendCommon(ctx, entry, isAuth)
		if err != nil {
			errors = multierror.Append(errors, fmt.Errorf("cannot reload plugin on %q: %w", mount, err))
			continue
		}
		c.logger.Info("successfully reloaded plugin", "plugin", entry.Accessor, "path", entry.Path, "version", entry.Version)
	}
	return errors
}

// reloadPlugin reloads all mounted backends that are of
// plugin pluginName (name of the plugin as registered in
// the plugin catalog).
func (c *Core) reloadMatchingPlugin(ctx context.Context, ns *namespace.Namespace, pluginType consts.PluginType, pluginName string) (int, error) {
	var reloaded int
	var secrets, auth, database bool
	switch pluginType {
	case consts.PluginTypeSecrets:
		secrets = true
	case consts.PluginTypeCredential:
		auth = true
	case consts.PluginTypeDatabase:
		database = true
	case consts.PluginTypeUnknown:
		secrets = true
		auth = true
		database = true
	}
	typeExists := map[consts.PluginType]bool{
		consts.PluginTypeCredential: false,
		consts.PluginTypeDatabase:   false,
		consts.PluginTypeSecrets:    false,
	}
	for pluginType := range typeExists {
		plugins, err := c.pluginCatalog.ListVersionedPlugins(ctx, pluginType)
		if err != nil {
			return reloaded, err
		}
		for _, plugin := range plugins {
			if plugin.Name == pluginName {
				typeExists[pluginType] = true
				break
			}
		}
	}
	secrets = secrets && typeExists[consts.PluginTypeSecrets]
	auth = auth && typeExists[consts.PluginTypeCredential]
	database = database && typeExists[consts.PluginTypeDatabase]

	if secrets || database {
		c.mountsLock.RLock()
		defer c.mountsLock.RUnlock()

		for _, entry := range c.mounts.Entries {
			// We dont reload mounts that are not in the same namespace
			if ns != nil && ns.ID != entry.Namespace().ID {
				continue
			}

			if entry.Type == pluginName || (entry.Type == "plugin" && entry.Config.PluginName == pluginName) {
				if !secrets {
					continue
				}
				err := c.reloadBackendCommon(ctx, entry, false)
				if err != nil {
					return reloaded, err
				}
				reloaded++
				c.logger.Info("successfully reloaded plugin", "plugin", pluginName, "namespace", entry.Namespace(), "path", entry.Path, "version", entry.Version)
			}

			// Knowledge of whether a database plugin is in use within a particular
			// database mount is internal to the mount, so we delegate the reload
			// request with an internally routed request.
			if database && entry.Type == "database" {
				reqCtx := namespace.ContextWithNamespace(ctx, entry.namespace)
				req := &logical.Request{
					Operation: logical.UpdateOperation,
					Path:      entry.Path + "reload/" + pluginName,
				}
				resp, err := c.router.Route(reqCtx, req)
				if err != nil {
					return reloaded, err
				}
				if resp == nil {
					return reloaded, fmt.Errorf("failed to reload %q database plugin(s) mounted under %s", pluginName, entry.Path)
				}
				if resp.IsError() {
					return reloaded, fmt.Errorf("failed to reload %q database plugin(s) mounted under %s: %s", pluginName, entry.Path, resp.Error())
				}

				if count, ok := resp.Data["count"].(int); ok && count > 0 {
					c.logger.Info("successfully reloaded database plugin(s)", "plugin", pluginName, "namespace", entry.Namespace(), "path", entry.Path, "connections", resp.Data["connections"])
					reloaded += count
				}
			}
		}
	}

	if auth {
		c.authLock.RLock()
		defer c.authLock.RUnlock()

		for _, entry := range c.auth.Entries {
			// We dont reload mounts that are not in the same namespace
			if ns != nil && ns.ID != entry.Namespace().ID {
				continue
			}

			if entry.Type == pluginName || (entry.Type == "plugin" && entry.Config.PluginName == pluginName) {
				err := c.reloadBackendCommon(ctx, entry, true)
				if err != nil {
					return reloaded, err
				}
				reloaded++
				c.logger.Info("successfully reloaded plugin", "plugin", entry.Accessor, "namespace", entry.Namespace(), "path", entry.Path, "version", entry.Version)
			}
		}
	}

	return reloaded, nil
}

// reloadBackendCommon is a generic method to reload a backend provided a
// MountEntry.
func (c *Core) reloadBackendCommon(ctx context.Context, entry *MountEntry, isAuth bool) error {
	// Make sure our cache is up-to-date. Since some singleton mounts can be
	// tuned, we do this before the below check.
	entry.SyncCache()

	// We don't want to reload the singleton mounts. They often have specific
	// inmemory elements and we don't want to touch them here.
	if strutil.StrListContains(singletonMounts, entry.Type) {
		c.logger.Debug("skipping reload of singleton mount", "type", entry.Type)
		return nil
	}

	path := entry.Path

	if isAuth {
		path = credentialRoutePrefix + path
	}

	// Fast-path out if the backend doesn't exist
	raw, ok := c.router.root.Get(entry.Namespace().Path + path)
	if !ok {
		return nil
	}

	re := raw.(*routeEntry)

	// Grab the lock, this allows requests to drain before we cleanup the
	// client.
	re.l.Lock()
	defer re.l.Unlock()

	// Only call Cleanup if backend is initialized
	if re.backend != nil {
		// Pass a context value so that the plugin client will call the
		// appropriate cleanup method for reloading
		reloadCtx := context.WithValue(ctx, plugin.ContextKeyPluginReload, "reload")
		// Call backend's Cleanup routine
		re.backend.Cleanup(reloadCtx)
	}

	view := re.storageView
	viewPath := entry.UUID + "/"
	switch entry.Table {
	case mountTableType:
		viewPath = backendBarrierPrefix + viewPath
	case credentialTableType:
		viewPath = credentialBarrierPrefix + viewPath
	}

	removePathCheckers(c, entry, viewPath)

	sysView := c.mountEntrySysView(entry)

	nilMount, err := preprocessMount(c, entry, view.(*BarrierView))
	if err != nil {
		return err
	}

	var backend logical.Backend
	oldSha := entry.RunningSha256
	if !isAuth {
		// Dispense a new backend
		backend, entry.RunningSha256, err = c.newLogicalBackend(ctx, entry, sysView, view)
	} else {
		backend, entry.RunningSha256, err = c.newCredentialBackend(ctx, entry, sysView, view)
	}
	if err != nil {
		return err
	}
	if backend == nil {
		return fmt.Errorf("nil backend of type %q returned from creation function", entry.Type)
	}

	// update the entry running version with the configured version, which was verified during registration.
	entry.RunningVersion = entry.Version
	if entry.RunningVersion == "" {
		// don't set the running version to a builtin if it is running as an external plugin
		if entry.RunningSha256 == "" {
			if isAuth {
				entry.RunningVersion = versions.GetBuiltinVersion(consts.PluginTypeCredential, entry.Type)
			} else {
				entry.RunningVersion = versions.GetBuiltinVersion(consts.PluginTypeSecrets, entry.Type)
			}
		}
	}

	// update the mount table since we changed the runningSha
	if oldSha != entry.RunningSha256 && MountTableUpdateStorage {
		if isAuth {
			err = c.persistAuth(ctx, c.auth, &entry.Local)
			if err != nil {
				return err
			}
		} else {
			err = c.persistMounts(ctx, c.mounts, &entry.Local)
			if err != nil {
				return err
			}
		}
	}
	addPathCheckers(c, entry, backend, viewPath)

	if nilMount {
		backend.Cleanup(ctx)
		backend = nil
	}

	// Set the backend back
	re.backend = backend

	if backend != nil {
		// Initialize the backend after reload. This is a no-op for backends < v5 which
		// rely on lazy loading for initialization. v5 backends do not rely on lazy loading
		// for initialization unless the plugin process is killed. Reload of a v5 backend
		// results in a new plugin process, so we must initialize the backend here.
		err := backend.Initialize(ctx, &logical.InitializationRequest{Storage: view})
		if err != nil {
			return err
		}

		// Set paths as well
		paths := backend.SpecialPaths()
		if paths != nil {
			re.rootPaths.Store(pathsToRadix(paths.Root))
			loginPathsEntry, err := parseUnauthenticatedPaths(paths.Unauthenticated)
			if err != nil {
				return err
			}
			re.loginPaths.Store(loginPathsEntry)
			binaryPathsEntry, err := parseUnauthenticatedPaths(paths.Binary)
			if err != nil {
				return err
			}
			re.binaryPaths.Store(binaryPathsEntry)
		}
	}

	return nil
}

func (c *Core) setupPluginReload() error {
	return handleSetupPluginReload(c)
}

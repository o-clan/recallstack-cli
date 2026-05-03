import {
  getEffectiveBaseUrl,
  loadConfig,
  updateProfileForBaseUrl,
  updateProfileForBaseUrlIfCurrent,
  type RecallstackConfig,
} from "./config.js";
import { HttpError, http } from "./http.js";

export async function loginWithCode(code: string, runtimeConfig?: RecallstackConfig): Promise<void> {
  const normalized = code.trim();
  if (!normalized.length) {
    throw new Error("Sign-in code is required.");
  }

  const cfg = runtimeConfig || loadConfig();
  const baseUrl = getEffectiveBaseUrl(cfg);

  const exchanged = await http<{
    access_token: string;
    refresh_token: string;
  }>(baseUrl, "/v1/auth/cli/code/exchange", {
    method: "POST",
    body: {
      code: normalized,
    },
  });

  updateProfileForBaseUrl(baseUrl, {
    accessToken: exchanged.access_token,
    refreshToken: exchanged.refresh_token,
    apiKey: undefined,
  });
}

export async function logout(
  runtimeConfig?: RecallstackConfig,
): Promise<{ remoteRevoked: boolean; warning?: string }> {
  const cfg = runtimeConfig || loadConfig();
  const baseUrl = getEffectiveBaseUrl(cfg);
  let remoteRevoked = false;
  let warning: string | undefined;

  if (cfg.accessToken) {
    try {
      const body = cfg.refreshToken ? { refresh_token: cfg.refreshToken } : {};
      await http<{ status: string }>(baseUrl, "/v1/auth/logout", {
        method: "POST",
        token: cfg.accessToken,
        body,
      });
      remoteRevoked = true;
    } catch (error) {
      warning = `Remote session revoke failed: ${error instanceof Error ? error.message : String(error)}`;
    }
  } else if (cfg.refreshToken) {
    warning = "Remote session revoke skipped: missing access token.";
  }

  updateProfileForBaseUrl(baseUrl, {
    accessToken: undefined,
    refreshToken: undefined,
    apiKey: undefined,
  });

  return {
    remoteRevoked,
    warning,
  };
}

async function refreshAccessToken(cfg: RecallstackConfig, baseUrl: string): Promise<RecallstackConfig> {
  if (!cfg.refreshToken) {
    throw new Error("Not authenticated. Run `recallstack login`.");
  }

  try {
    const refreshed = await http<{
      access_token: string;
      refresh_token: string;
      expires_in: number;
    }>(baseUrl, "/v1/auth/token/refresh", {
      method: "POST",
      body: {
        refresh_token: cfg.refreshToken,
      },
    });

    const nextConfig: RecallstackConfig = {
      ...cfg,
      accessToken: refreshed.access_token,
      refreshToken: refreshed.refresh_token,
    };
    updateProfileForBaseUrl(baseUrl, {
      accessToken: refreshed.access_token,
      refreshToken: refreshed.refresh_token,
    });
    return nextConfig;
  } catch (error) {
    if (error instanceof HttpError && error.status === 401) {
      const latest = latestConfigForBaseUrl(baseUrl);
      if (latest && configHasNewerAuth(latest, cfg)) {
        return latest;
      }
      updateProfileForBaseUrlIfCurrent(
        baseUrl,
        { refreshToken: cfg.refreshToken },
        {
          accessToken: undefined,
          refreshToken: undefined,
        },
      );
      throw new Error("Authentication expired. Run `recallstack login`.");
    }
    throw error;
  }
}

function latestConfigForBaseUrl(baseUrl: string): RecallstackConfig | undefined {
  const latest = loadConfig();
  return getEffectiveBaseUrl(latest) === baseUrl ? latest : undefined;
}

function configHasNewerAuth(latest: RecallstackConfig, previous: RecallstackConfig): boolean {
  return Boolean(
    (latest.accessToken && latest.accessToken !== previous.accessToken)
    || (latest.refreshToken && latest.refreshToken !== previous.refreshToken),
  );
}

async function retryWithFreshAuth<T>(
  baseUrl: string,
  previousConfig: RecallstackConfig,
  path: string,
  options: { method?: string; body?: unknown; workspaceId?: string },
): Promise<T | undefined> {
  const latest = latestConfigForBaseUrl(baseUrl);
  if (!latest || !configHasNewerAuth(latest, previousConfig)) {
    return undefined;
  }

  if (latest.accessToken) {
    return http<T>(baseUrl, path, {
      method: options.method,
      body: options.body,
      token: latest.accessToken,
      workspaceId: options.workspaceId,
    });
  }

  if (latest.refreshToken) {
    const refreshed = await refreshAccessToken(latest, baseUrl);
    if (refreshed.accessToken) {
      return http<T>(baseUrl, path, {
        method: options.method,
        body: options.body,
        token: refreshed.accessToken,
        workspaceId: options.workspaceId,
      });
    }
  }

  return undefined;
}

export async function authenticatedHttp<T>(
  path: string,
  options: { method?: string; body?: unknown; workspaceId?: string } = {},
  runtimeConfig?: RecallstackConfig,
): Promise<T> {
  const cfg = runtimeConfig || loadConfig();
  const baseUrl = getEffectiveBaseUrl(cfg);

  if (cfg.accessToken) {
    try {
      return await http<T>(baseUrl, path, {
        method: options.method,
        body: options.body,
        token: cfg.accessToken,
        workspaceId: options.workspaceId,
      });
    } catch (error) {
      if (!(error instanceof HttpError) || error.status !== 401) {
        throw error;
      }

      if (cfg.refreshToken) {
        try {
          const refreshed = await refreshAccessToken(cfg, baseUrl);
          if (!refreshed.accessToken) {
            throw new Error("Not authenticated. Run `recallstack login`.");
          }
          return await http<T>(baseUrl, path, {
            method: options.method,
            body: options.body,
            token: refreshed.accessToken,
            workspaceId: options.workspaceId,
          });
        } catch (refreshError) {
          const retried = await retryWithFreshAuth<T>(baseUrl, cfg, path, options);
          if (retried !== undefined) {
            return retried;
          }
          if (cfg.apiKey) {
            return http<T>(baseUrl, path, {
              method: options.method,
              body: options.body,
              token: cfg.apiKey,
              workspaceId: options.workspaceId,
            });
          }
          throw refreshError;
        }
      }

      if (cfg.apiKey) {
        return http<T>(baseUrl, path, {
          method: options.method,
          body: options.body,
          token: cfg.apiKey,
          workspaceId: options.workspaceId,
        });
      }

      const retried = await retryWithFreshAuth<T>(baseUrl, cfg, path, options);
      if (retried !== undefined) {
        return retried;
      }

      throw error;
    }
  }

  if (cfg.apiKey) {
    return http<T>(baseUrl, path, {
      method: options.method,
      body: options.body,
      token: cfg.apiKey,
      workspaceId: options.workspaceId,
    });
  }

  throw new Error("Not authenticated. Run `recallstack login`.");
}

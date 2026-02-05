use serde::{Deserialize, Serialize};
use varnish::vcl::{Ctx, LogTag, StrOrBytes, VclBackend, VclError, VclResponse};

use crate::config::RequestRedirectFilter;

/// Stateless redirect backend - actual redirect config passed via request header
pub struct RedirectBackend;

/// Redirect configuration stored in request header
#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct RedirectConfig {
    pub filter: RequestRedirectFilter,
    // Original request components
    pub original_scheme: String,
    pub original_hostname: String,
    pub original_port: u16,
    pub original_path: String,
    pub original_query: String,
    // Matched path for prefix replacement (string value of matched prefix)
    pub matched_path: Option<String>,
}

impl VclBackend<RedirectBody> for RedirectBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<RedirectBody>, VclError> {
        // Read redirect config from internal header (immutable borrow)
        let config_json = {
            let bereq = ctx
                .http_bereq
                .as_ref()
                .ok_or_else(|| VclError::new("Missing bereq in redirect backend".to_string()))?;

            bereq
                .header("X-Ghost-Redirect-Config")
                .and_then(|h| match h {
                    StrOrBytes::Utf8(s) => Some(s.to_string()),
                    StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok().map(|s| s.to_string()),
                })
                .ok_or_else(|| VclError::new("Missing redirect config header".to_string()))?
        };

        let config: RedirectConfig = serde_json::from_str(&config_json).map_err(|e| {
            ctx.log(
                LogTag::Error,
                &format!("Invalid redirect config: {}", e),
            );
            VclError::new(format!("Invalid redirect config: {}", e))
        })?;

        // Build Location header
        let location = build_location(&config).map_err(|e| {
            ctx.log(
                LogTag::Error,
                &format!("Failed to build location: {}", e),
            );
            e
        })?;

        // Set response status and Location header
        {
            let beresp = ctx.http_beresp.as_mut().ok_or_else(|| {
                VclError::new("Missing beresp in redirect backend".to_string())
            })?;

            beresp.set_status(config.filter.status_code as u16);
            beresp.set_header("Location", &location)?;
        }

        // Remove internal header (mutable borrow)
        {
            let bereq = ctx
                .http_bereq
                .as_mut()
                .ok_or_else(|| VclError::new("Missing bereq in redirect backend".to_string()))?;
            bereq.unset_header("X-Ghost-Redirect-Config");
        }

        ctx.log(
            LogTag::Debug,
            &format!("Redirect {} -> {}", config.filter.status_code, location),
        );

        Ok(Some(RedirectBody::new()))
    }
}

/// Build Location header from redirect config
pub fn build_location(config: &RedirectConfig) -> Result<String, VclError> {
    // Determine components (filter overrides original)
    let scheme = config
        .filter
        .scheme
        .as_deref()
        .unwrap_or(&config.original_scheme);
    let hostname = config
        .filter
        .hostname
        .as_deref()
        .unwrap_or(&config.original_hostname);

    // Port handling: if scheme changes without explicit port, use default port for new scheme
    let port = if let Some(explicit_port) = config.filter.port {
        explicit_port
    } else if config.filter.scheme.is_some() && config.filter.scheme.as_deref() != Some(&config.original_scheme) {
        // Scheme changed without explicit port - use default for new scheme
        if scheme == "https" { 443 } else { 80 }
    } else {
        // No scheme change or scheme not specified - keep original port
        config.original_port
    };

    // Rewrite path if specified
    let path = rewrite_path(
        &config.filter,
        &config.original_path,
        config.matched_path.as_deref(),
    )?;

    // Construct Location: scheme://hostname[:port]/path[?query]
    let mut location = format!("{}://{}", scheme, hostname);

    // Include port unless it's a default port
    if !should_omit_port(scheme, port) {
        location.push_str(&format!(":{}", port));
    }

    // Add path
    location.push_str(&path);

    // Preserve query string
    if !config.original_query.is_empty() {
        location.push('?');
        location.push_str(&config.original_query);
    }

    Ok(location)
}

fn should_omit_port(scheme: &str, port: u16) -> bool {
    (scheme == "http" && port == 80) || (scheme == "https" && port == 443)
}

fn rewrite_path(
    filter: &RequestRedirectFilter,
    original_path: &str,
    matched_path_str: Option<&str>,
) -> Result<String, VclError> {
    // ReplaceFullPath: simple replacement
    if let Some(full_path) = &filter.replace_full_path {
        return Ok(full_path.clone());
    }

    // ReplacePrefixMatch: replace matched prefix with new prefix
    if let Some(new_prefix) = &filter.replace_prefix_match {
        if let Some(matched_prefix) = matched_path_str {
            // We have a matched prefix, use it
            if original_path.starts_with(matched_prefix) {
                let remainder = &original_path[matched_prefix.len()..];
                let trimmed_new = new_prefix.trim_end_matches('/');
                return Ok(if remainder.is_empty() {
                    trimmed_new.to_string()
                } else if remainder.starts_with('/') {
                    format!("{}{}", trimmed_new, remainder)
                } else {
                    format!("{}/{}", trimmed_new, remainder)
                });
            }
        }

        // Fallback: use first-segment heuristic (same as vhost_director.rs)
        return Ok(replace_first_segment_heuristic(original_path, new_prefix));
    }

    Ok(original_path.to_string())
}

/// Simplified version of vhost_director.rs:replace_first_segment_heuristic
fn replace_first_segment_heuristic(path: &str, new_prefix: &str) -> String {
    if path.starts_with('/') {
        let segments: Vec<&str> = path.splitn(3, '/').collect();
        if segments.len() >= 2 {
            let remainder = if segments.len() > 2 { segments[2] } else { "" };
            let trimmed_new = new_prefix.trim_end_matches('/');
            if remainder.is_empty() {
                trimmed_new.to_string()
            } else {
                format!("{}/{}", trimmed_new, remainder)
            }
        } else {
            new_prefix.to_string()
        }
    } else {
        new_prefix.to_string()
    }
}

pub struct RedirectBody {
    #[allow(dead_code)]
    cursor: usize,
}

impl RedirectBody {
    pub fn new() -> Self {
        Self { cursor: 0 }
    }
}

impl Default for RedirectBody {
    fn default() -> Self {
        Self::new()
    }
}

impl VclResponse for RedirectBody {
    fn read(&mut self, _buf: &mut [u8]) -> Result<usize, VclError> {
        // Empty body - return 0 immediately
        Ok(0)
    }

    fn len(&self) -> Option<usize> {
        Some(0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_config(
        filter: RequestRedirectFilter,
        original_scheme: &str,
        original_hostname: &str,
        original_port: u16,
        original_path: &str,
        original_query: &str,
    ) -> RedirectConfig {
        RedirectConfig {
            filter,
            original_scheme: original_scheme.to_string(),
            original_hostname: original_hostname.to_string(),
            original_port,
            original_path: original_path.to_string(),
            original_query: original_query.to_string(),
            matched_path: None,
        }
    }

    fn make_filter(
        scheme: Option<&str>,
        hostname: Option<&str>,
        port: Option<u16>,
        replace_full_path: Option<&str>,
        replace_prefix_match: Option<&str>,
        status_code: u16,
    ) -> RequestRedirectFilter {
        RequestRedirectFilter {
            scheme: scheme.map(|s| s.to_string()),
            hostname: hostname.map(|s| s.to_string()),
            path_type: None,
            replace_full_path: replace_full_path.map(|s| s.to_string()),
            replace_prefix_match: replace_prefix_match.map(|s| s.to_string()),
            port,
            status_code,
        }
    }

    #[test]
    fn test_build_location_basic() {
        let config = make_config(
            make_filter(Some("https"), None, None, None, None, 301),
            "http",
            "example.com",
            80,
            "/path",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "https://example.com/path");
    }

    #[test]
    fn test_build_location_default_ports() {
        // HTTPS with default port 443 - should omit
        let config = make_config(
            make_filter(Some("https"), None, Some(443), None, None, 301),
            "http",
            "example.com",
            80,
            "/path",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "https://example.com/path");

        // HTTP with default port 80 - should omit
        let config = make_config(
            make_filter(Some("http"), None, Some(80), None, None, 301),
            "http",
            "example.com",
            8080,
            "/path",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "http://example.com/path");

        // Non-default port - should include
        let config = make_config(
            make_filter(Some("https"), None, Some(8443), None, None, 301),
            "http",
            "example.com",
            80,
            "/path",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "https://example.com:8443/path");
    }

    #[test]
    fn test_build_location_path_rewrite_full() {
        let config = make_config(
            make_filter(None, None, None, Some("/new/path"), None, 301),
            "http",
            "example.com",
            80,
            "/old/path",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "http://example.com/new/path");
    }

    #[test]
    fn test_build_location_path_rewrite_prefix() {
        let mut config = make_config(
            make_filter(None, None, None, None, Some("/v2"), 301),
            "http",
            "example.com",
            80,
            "/v1/users/123",
            "",
        );
        config.matched_path = Some("/v1".to_string());

        let location = build_location(&config).unwrap();
        assert_eq!(location, "http://example.com/v2/users/123");
    }

    #[test]
    fn test_build_location_query_preservation() {
        let config = make_config(
            make_filter(Some("https"), None, None, None, None, 301),
            "http",
            "example.com",
            80,
            "/path",
            "key=value&other=123",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "https://example.com/path?key=value&other=123");
    }

    #[test]
    fn test_build_location_percent_encoding() {
        let config = make_config(
            make_filter(Some("https"), None, None, None, None, 301),
            "http",
            "example.com",
            80,
            "/path%20with%20spaces",
            "q=hello%20world",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(
            location,
            "https://example.com/path%20with%20spaces?q=hello%20world"
        );
    }

    #[test]
    fn test_build_location_hostname_change() {
        let config = make_config(
            make_filter(None, Some("new.example.com"), None, None, None, 302),
            "http",
            "old.example.com",
            80,
            "/api",
            "",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "http://new.example.com/api");
    }

    #[test]
    fn test_build_location_combined() {
        let config = make_config(
            make_filter(
                Some("https"),
                Some("new.example.com"),
                Some(443),
                Some("/v2/api"),
                None,
                301,
            ),
            "http",
            "old.example.com",
            8080,
            "/v1/api",
            "token=abc",
        );

        let location = build_location(&config).unwrap();
        assert_eq!(location, "https://new.example.com/v2/api?token=abc");
    }

    #[test]
    fn test_rewrite_path_prefix_with_trailing_slash() {
        let filter = make_filter(None, None, None, None, Some("/v2/"), 301);

        let result = rewrite_path(&filter, "/v1/users", Some("/v1")).unwrap();
        assert_eq!(result, "/v2/users");
    }

    #[test]
    fn test_rewrite_path_empty_remainder() {
        let filter = make_filter(None, None, None, None, Some("/v2"), 301);

        let result = rewrite_path(&filter, "/v1", Some("/v1")).unwrap();
        assert_eq!(result, "/v2");
    }

    #[test]
    fn test_should_omit_port() {
        assert!(should_omit_port("http", 80));
        assert!(should_omit_port("https", 443));
        assert!(!should_omit_port("http", 8080));
        assert!(!should_omit_port("https", 8443));
        assert!(!should_omit_port("http", 443));
        assert!(!should_omit_port("https", 80));
    }
}

//! Configuration loading and parsing for Ghost VMOD

use serde::Deserialize;
use std::collections::HashMap;
use std::fs;
use std::path::Path;

/// Backend endpoint definition
#[derive(Debug, Clone, Deserialize)]
pub struct Backend {
    pub address: String,
    pub port: u16,
    #[serde(default = "default_weight")]
    pub weight: u32,
}

fn default_weight() -> u32 {
    100
}

/// Virtual host configuration
#[derive(Debug, Clone, Deserialize)]
pub struct VHost {
    pub backends: Vec<Backend>,
}

/// Phase 1 configuration schema
#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub version: u32,
    #[serde(default)]
    pub vhosts: HashMap<String, VHost>,
    pub default: Option<VHost>,
}

/// Path match type for routing rules
#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "PascalCase")]
pub enum PathMatchType {
    Exact,
    PathPrefix,
    RegularExpression,
}

/// Path matching rule
#[derive(Debug, Clone, Deserialize)]
pub struct PathMatch {
    #[serde(rename = "type")]
    pub match_type: PathMatchType,
    pub value: String,
}

/// Route with path-based matching (v2)
#[derive(Debug, Clone, Deserialize)]
pub struct Route {
    pub path_match: Option<PathMatch>,
    pub backends: Vec<Backend>,
    pub priority: i32,
}

/// Virtual host configuration with path-based routing (v2)
#[derive(Debug, Clone, Deserialize)]
pub struct VHostV2 {
    pub routes: Vec<Route>,
    #[serde(default)]
    pub default_backends: Vec<Backend>,
}

/// Phase 2 configuration schema with path-based routing
#[derive(Debug, Clone, Deserialize)]
pub struct ConfigV2 {
    pub version: u32,
    #[serde(default)]
    pub vhosts: HashMap<String, VHostV2>,
    pub default: Option<VHost>,
}

/// Load configuration from a JSON file.
/// If the file doesn't exist, returns an empty config (useful for startup before
/// the config is generated).
pub fn load(path: &Path) -> Result<Config, String> {
    // If file doesn't exist, return empty config
    if !path.exists() {
        return Ok(Config::empty());
    }

    let content = fs::read_to_string(path)
        .map_err(|e| format!("failed to read config file {}: {}", path.display(), e))?;

    let config: Config = serde_json::from_str(&content)
        .map_err(|e| format!("failed to parse config file {}: {}", path.display(), e))?;

    validate(&config)?;

    Ok(config)
}

impl Config {
    /// Create an empty configuration with no vhosts.
    /// Used when the config file doesn't exist yet at startup.
    pub fn empty() -> Self {
        Config {
            version: 1,
            vhosts: HashMap::new(),
            default: None,
        }
    }
}

impl ConfigV2 {
    /// Create an empty v2 configuration with no vhosts.
    pub fn empty() -> Self {
        ConfigV2 {
            version: 2,
            vhosts: HashMap::new(),
            default: None,
        }
    }
}

/// Load v2 configuration from a JSON file.
pub fn load_v2(path: &Path) -> Result<ConfigV2, String> {
    // If file doesn't exist, return empty config
    if !path.exists() {
        return Ok(ConfigV2::empty());
    }

    let content = fs::read_to_string(path)
        .map_err(|e| format!("failed to read config file {}: {}", path.display(), e))?;

    let config: ConfigV2 = serde_json::from_str(&content)
        .map_err(|e| format!("failed to parse config file {}: {}", path.display(), e))?;

    validate_v2(&config)?;

    Ok(config)
}

/// Validate configuration
fn validate(config: &Config) -> Result<(), String> {
    if config.version != 1 {
        return Err(format!(
            "unsupported config version: {} (expected 1)",
            config.version
        ));
    }

    for (hostname, vhost) in &config.vhosts {
        validate_hostname(hostname)?;
        validate_backends(hostname, &vhost.backends)?;
    }

    if let Some(ref default) = config.default {
        validate_backends("default", &default.backends)?;
    }

    Ok(())
}

/// Validate hostname format
fn validate_hostname(hostname: &str) -> Result<(), String> {
    if hostname.is_empty() {
        return Err("hostname cannot be empty".to_string());
    }

    // Check for valid wildcard pattern
    if hostname.contains('*') {
        // Only allow leading wildcard: *.example.com
        if !hostname.starts_with("*.") {
            return Err(format!(
                "invalid wildcard hostname '{}': wildcard must be at start (*.example.com)",
                hostname
            ));
        }
        // No other wildcards allowed
        if hostname[2..].contains('*') {
            return Err(format!(
                "invalid wildcard hostname '{}': only single leading wildcard allowed",
                hostname
            ));
        }
    }

    Ok(())
}

/// Validate backend list
fn validate_backends(context: &str, backends: &[Backend]) -> Result<(), String> {
    for (i, backend) in backends.iter().enumerate() {
        if backend.address.is_empty() {
            return Err(format!(
                "backend {} in '{}': address cannot be empty",
                i, context
            ));
        }
        if backend.port == 0 {
            return Err(format!("backend {} in '{}': port cannot be 0", i, context));
        }
        if backend.weight == 0 {
            return Err(format!(
                "backend {} in '{}': weight cannot be 0",
                i, context
            ));
        }
    }
    Ok(())
}

/// Validate v2 configuration
fn validate_v2(config: &ConfigV2) -> Result<(), String> {
    if config.version != 2 {
        return Err(format!(
            "unsupported config version: {} (expected 2)",
            config.version
        ));
    }

    for (hostname, vhost) in &config.vhosts {
        validate_hostname(hostname)?;

        for (i, route) in vhost.routes.iter().enumerate() {
            let route_ctx = format!("{} route {}", hostname, i);
            validate_backends(&route_ctx, &route.backends)?;

            if let Some(ref path_match) = route.path_match {
                validate_path_match(path_match, &route_ctx)?;
            }
        }

        if !vhost.default_backends.is_empty() {
            validate_backends(&format!("{} default_backends", hostname), &vhost.default_backends)?;
        }
    }

    if let Some(ref default) = config.default {
        validate_backends("default", &default.backends)?;
    }

    Ok(())
}

/// Validate path match configuration
fn validate_path_match(path_match: &PathMatch, context: &str) -> Result<(), String> {
    match path_match.match_type {
        PathMatchType::Exact | PathMatchType::PathPrefix => {
            // Paths must start with /
            if !path_match.value.starts_with('/') {
                return Err(format!(
                    "{}: path '{}' must start with /",
                    context, path_match.value
                ));
            }
            // No consecutive slashes
            if path_match.value.contains("//") {
                return Err(format!(
                    "{}: path '{}' cannot contain consecutive slashes",
                    context, path_match.value
                ));
            }
        }
        PathMatchType::RegularExpression => {
            // Regex patterns have a max length
            if path_match.value.len() > 1024 {
                return Err(format!(
                    "{}: regex pattern too long ({} chars, max 1024)",
                    context,
                    path_match.value.len()
                ));
            }
            // Try to compile it to validate syntax
            if let Err(e) = regex::Regex::new(&path_match.value) {
                return Err(format!(
                    "{}: invalid regex pattern '{}': {}",
                    context, path_match.value, e
                ));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn write_config(content: &str) -> NamedTempFile {
        let mut file = NamedTempFile::new().unwrap();
        write!(file, "{}", content).unwrap();
        file
    }

    #[test]
    fn test_load_minimal_config() {
        let file = write_config(r#"{"version": 1}"#);
        let config = load(file.path()).unwrap();
        assert_eq!(config.version, 1);
        assert!(config.vhosts.is_empty());
        assert!(config.default.is_none());
    }

    #[test]
    fn test_load_nonexistent_file() {
        // Loading a non-existent file should return an empty config
        let path = Path::new("/nonexistent/ghost.json");
        let config = load(path).unwrap();
        assert_eq!(config.version, 1);
        assert!(config.vhosts.is_empty());
        assert!(config.default.is_none());
    }

    #[test]
    fn test_load_full_config() {
        let file = write_config(
            r#"{
            "version": 1,
            "vhosts": {
                "api.example.com": {
                    "backends": [
                        {"address": "10.0.0.1", "port": 8080, "weight": 100},
                        {"address": "10.0.0.2", "port": 8080, "weight": 50}
                    ]
                },
                "*.staging.example.com": {
                    "backends": [
                        {"address": "10.0.1.1", "port": 80}
                    ]
                }
            },
            "default": {
                "backends": [
                    {"address": "10.0.99.1", "port": 80}
                ]
            }
        }"#,
        );

        let config = load(file.path()).unwrap();
        assert_eq!(config.version, 1);
        assert_eq!(config.vhosts.len(), 2);
        assert!(config.vhosts.contains_key("api.example.com"));
        assert!(config.vhosts.contains_key("*.staging.example.com"));

        let api = &config.vhosts["api.example.com"];
        assert_eq!(api.backends.len(), 2);
        assert_eq!(api.backends[0].weight, 100);
        assert_eq!(api.backends[1].weight, 50);

        let staging = &config.vhosts["*.staging.example.com"];
        assert_eq!(staging.backends.len(), 1);
        assert_eq!(staging.backends[0].weight, 100); // default weight
    }

    #[test]
    fn test_invalid_version() {
        let file = write_config(r#"{"version": 0}"#);
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("unsupported config version"));
    }

    #[test]
    fn test_invalid_wildcard_middle() {
        let file =
            write_config(r#"{"version": 1, "vhosts": {"foo.*.example.com": {"backends": []}}}"#);
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("wildcard must be at start"));
    }

    #[test]
    fn test_invalid_wildcard_double() {
        let file =
            write_config(r#"{"version": 1, "vhosts": {"*.*.example.com": {"backends": []}}}"#);
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("only single leading wildcard"));
    }

    #[test]
    fn test_invalid_backend_empty_address() {
        let file = write_config(
            r#"{"version": 1, "vhosts": {"foo.com": {"backends": [{"address": "", "port": 80}]}}}"#,
        );
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("address cannot be empty"));
    }

    #[test]
    fn test_invalid_backend_zero_port() {
        let file = write_config(
            r#"{"version": 1, "vhosts": {"foo.com": {"backends": [{"address": "1.2.3.4", "port": 0}]}}}"#,
        );
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("port cannot be 0"));
    }

    #[test]
    fn test_invalid_backend_zero_weight() {
        let file = write_config(
            r#"{"version": 1, "vhosts": {"foo.com": {"backends": [{"address": "1.2.3.4", "port": 80, "weight": 0}]}}}"#,
        );
        let result = load(file.path());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("weight cannot be 0"));
    }
}

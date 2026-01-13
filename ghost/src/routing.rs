//! Host matching and backend selection for Ghost VMOD

use crate::config::{Backend, Config, VHost};

/// Result of vhost matching
pub enum MatchResult<'a> {
    /// Found a matching vhost with backends
    Found(&'a VHost),
    /// No vhost matched (404)
    NotFound,
    /// Matched but no backends available (503)
    NoBackends,
}

/// Match a hostname against the configuration
pub fn match_vhost<'a>(config: &'a Config, host: &str) -> MatchResult<'a> {
    let host = host.to_lowercase();

    // 1. Exact match
    if let Some(vhost) = config.vhosts.get(&host) {
        return if vhost.backends.is_empty() {
            MatchResult::NoBackends
        } else {
            MatchResult::Found(vhost)
        };
    }

    // 2. Wildcard match
    for (pattern, vhost) in &config.vhosts {
        if pattern.starts_with("*.") && matches_wildcard(pattern, &host) {
            return if vhost.backends.is_empty() {
                MatchResult::NoBackends
            } else {
                MatchResult::Found(vhost)
            };
        }
    }

    // 3. Default fallback
    match &config.default {
        Some(vhost) if !vhost.backends.is_empty() => MatchResult::Found(vhost),
        Some(_) => MatchResult::NoBackends,
        None => MatchResult::NotFound,
    }
}

/// Check if a wildcard pattern matches a hostname
/// Pattern: "*.example.com" matches "foo.example.com" but not "foo.bar.example.com"
/// Per Gateway API spec: single label wildcard only
fn matches_wildcard(pattern: &str, host: &str) -> bool {
    // Pattern: "*.example.com" -> suffix: ".example.com"
    let suffix = &pattern[1..];

    if !host.ends_with(suffix) {
        return false;
    }

    // Check the matched part has no dots (single label only)
    let prefix_len = host.len() - suffix.len();
    let prefix = &host[..prefix_len];

    // Prefix must be non-empty and contain no dots
    !prefix.is_empty() && !prefix.contains('.')
}

/// Select a backend using weighted random selection
pub fn select_backend(vhost: &VHost) -> Option<&Backend> {
    if vhost.backends.is_empty() {
        return None;
    }

    if vhost.backends.len() == 1 {
        return Some(&vhost.backends[0]);
    }

    // Calculate total weight
    let total_weight: u32 = vhost.backends.iter().map(|b| b.weight).sum();

    if total_weight == 0 {
        return None;
    }

    // Random selection
    use rand::Rng;
    let mut rng = rand::thread_rng();
    let r = rng.gen_range(0..total_weight);

    let mut cumulative = 0u32;
    for backend in &vhost.backends {
        cumulative += backend.weight;
        if r < cumulative {
            return Some(backend);
        }
    }

    // Fallback (shouldn't happen if weights are valid)
    Some(&vhost.backends[0])
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn make_vhost(backends: Vec<(&str, u16, u32)>) -> VHost {
        VHost {
            backends: backends
                .into_iter()
                .map(|(addr, port, weight)| Backend {
                    address: addr.to_string(),
                    port,
                    weight,
                })
                .collect(),
        }
    }

    fn make_config(vhosts: Vec<(&str, VHost)>, default: Option<VHost>) -> Config {
        Config {
            version: 1,
            vhosts: vhosts
                .into_iter()
                .map(|(k, v)| (k.to_string(), v))
                .collect(),
            default,
        }
    }

    #[test]
    fn test_exact_match() {
        let config = make_config(
            vec![("api.example.com", make_vhost(vec![("10.0.0.1", 80, 100)]))],
            None,
        );

        match match_vhost(&config, "api.example.com") {
            MatchResult::Found(vhost) => {
                assert_eq!(vhost.backends.len(), 1);
                assert_eq!(vhost.backends[0].address, "10.0.0.1");
            }
            _ => panic!("Expected Found"),
        }
    }

    #[test]
    fn test_exact_match_case_insensitive() {
        let config = make_config(
            vec![("api.example.com", make_vhost(vec![("10.0.0.1", 80, 100)]))],
            None,
        );

        match match_vhost(&config, "API.Example.COM") {
            MatchResult::Found(_) => {}
            _ => panic!("Expected Found"),
        }
    }

    #[test]
    fn test_wildcard_match() {
        let config = make_config(
            vec![(
                "*.staging.example.com",
                make_vhost(vec![("10.0.0.1", 80, 100)]),
            )],
            None,
        );

        // Should match
        match match_vhost(&config, "foo.staging.example.com") {
            MatchResult::Found(_) => {}
            _ => panic!("Expected Found for foo.staging.example.com"),
        }

        match match_vhost(&config, "bar.staging.example.com") {
            MatchResult::Found(_) => {}
            _ => panic!("Expected Found for bar.staging.example.com"),
        }
    }

    #[test]
    fn test_wildcard_single_label_only() {
        let config = make_config(
            vec![(
                "*.staging.example.com",
                make_vhost(vec![("10.0.0.1", 80, 100)]),
            )],
            None,
        );

        // Should NOT match - multiple labels
        match match_vhost(&config, "foo.bar.staging.example.com") {
            MatchResult::NotFound => {}
            _ => panic!("Expected NotFound for foo.bar.staging.example.com"),
        }
    }

    #[test]
    fn test_wildcard_requires_label() {
        let config = make_config(
            vec![("*.example.com", make_vhost(vec![("10.0.0.1", 80, 100)]))],
            None,
        );

        // Should NOT match - no prefix label
        match match_vhost(&config, ".example.com") {
            MatchResult::NotFound => {}
            _ => panic!("Expected NotFound for .example.com"),
        }
    }

    #[test]
    fn test_default_fallback() {
        let config = make_config(
            vec![("api.example.com", make_vhost(vec![("10.0.0.1", 80, 100)]))],
            Some(make_vhost(vec![("10.0.99.1", 80, 100)])),
        );

        match match_vhost(&config, "unknown.example.com") {
            MatchResult::Found(vhost) => {
                assert_eq!(vhost.backends[0].address, "10.0.99.1");
            }
            _ => panic!("Expected Found (default)"),
        }
    }

    #[test]
    fn test_no_match_no_default() {
        let config = make_config(
            vec![("api.example.com", make_vhost(vec![("10.0.0.1", 80, 100)]))],
            None,
        );

        match match_vhost(&config, "unknown.example.com") {
            MatchResult::NotFound => {}
            _ => panic!("Expected NotFound"),
        }
    }

    #[test]
    fn test_empty_backends() {
        let config = make_config(vec![("api.example.com", make_vhost(vec![]))], None);

        match match_vhost(&config, "api.example.com") {
            MatchResult::NoBackends => {}
            _ => panic!("Expected NoBackends"),
        }
    }

    #[test]
    fn test_select_backend_single() {
        let vhost = make_vhost(vec![("10.0.0.1", 80, 100)]);
        let backend = select_backend(&vhost).unwrap();
        assert_eq!(backend.address, "10.0.0.1");
    }

    #[test]
    fn test_select_backend_weighted_distribution() {
        let vhost = make_vhost(vec![("10.0.0.1", 80, 90), ("10.0.0.2", 80, 10)]);

        // Run many selections and check distribution
        let mut counts = HashMap::new();
        for _ in 0..1000 {
            let backend = select_backend(&vhost).unwrap();
            *counts.entry(backend.address.clone()).or_insert(0) += 1;
        }

        // With 90/10 weights, 10.0.0.1 should be selected ~90% of the time
        let count_1 = *counts.get("10.0.0.1").unwrap_or(&0);
        let count_2 = *counts.get("10.0.0.2").unwrap_or(&0);

        // Allow for statistical variance (should be roughly 900:100)
        assert!(
            count_1 > 800,
            "10.0.0.1 selected {} times, expected ~900",
            count_1
        );
        assert!(
            count_2 < 200,
            "10.0.0.2 selected {} times, expected ~100",
            count_2
        );
    }

    #[test]
    fn test_select_backend_empty() {
        let vhost = make_vhost(vec![]);
        assert!(select_backend(&vhost).is_none());
    }
}

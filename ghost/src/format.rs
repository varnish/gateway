//! Formatting utilities for backend.list output
//!
//! Provides helpers for formatting timestamps, percentages, and table-like output
//! for the varnishadm backend.list command.

use std::time::SystemTime;
use time::{format_description::well_known::Rfc3339, OffsetDateTime};

/// Format SystemTime as RFC3339 timestamp or "never"
///
/// # Arguments
///
/// * `time` - Optional SystemTime to format
///
/// # Returns
///
/// RFC3339 formatted timestamp string, or "never" if None
///
/// # Example
///
/// ```
/// use std::time::{SystemTime, Duration, UNIX_EPOCH};
/// # use vmod_ghost::format::format_timestamp;
///
/// let time = UNIX_EPOCH + Duration::from_secs(1737820245);
/// let formatted = format_timestamp(Some(time));
/// assert!(formatted.contains("2025-01-25"));
/// ```
pub fn format_timestamp(time: Option<SystemTime>) -> String {
    match time {
        Some(t) => {
            let dt: OffsetDateTime = t.into();
            dt.format(&Rfc3339)
                .unwrap_or_else(|_| "invalid".to_string())
        }
        None => "never".to_string(),
    }
}

/// Format percentage with 1 decimal place
///
/// # Arguments
///
/// * `count` - The numerator
/// * `total` - The denominator
///
/// # Returns
///
/// Formatted percentage string (e.g., "75.0%")
///
/// # Example
///
/// ```
/// # use vmod_ghost::format::format_percentage;
/// assert_eq!(format_percentage(75, 100), "75.0%");
/// assert_eq!(format_percentage(1, 3), "33.3%");
/// assert_eq!(format_percentage(0, 0), "0.0%");
/// ```
pub fn format_percentage(count: u64, total: u64) -> String {
    if total == 0 {
        "0.0%".to_string()
    } else {
        format!("{:.1}%", (count as f64 / total as f64) * 100.0)
    }
}

/// Format backend selections as JSON array for backend.list -j output
///
/// # Arguments
///
/// * `selections` - HashMap of backend addresses to selection counts
/// * `total` - Total number of requests for percentage calculation
///
/// # Returns
///
/// Vec of JSON objects with address, selections, and percentage fields
///
/// # Example
///
/// ```
/// use std::collections::HashMap;
/// # use vmod_ghost::format::format_backend_selections_json;
///
/// let mut selections = HashMap::new();
/// selections.insert("10.0.0.1:8080".to_string(), 75);
/// selections.insert("10.0.0.2:8080".to_string(), 25);
///
/// let backends = format_backend_selections_json(&selections, 100);
/// assert_eq!(backends.len(), 2);
/// ```
pub fn format_backend_selections_json(
    selections: &std::collections::HashMap<String, u64>,
    total: u64,
) -> Vec<serde_json::Value> {
    selections
        .iter()
        .map(|(key, count)| {
            serde_json::json!({
                "address": key,
                "selections": count,
                "percentage": if total > 0 {
                    (*count as f64 / total as f64) * 100.0
                } else {
                    0.0
                }
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, UNIX_EPOCH};

    #[test]
    fn test_format_timestamp_none() {
        assert_eq!(format_timestamp(None), "never");
    }

    #[test]
    fn test_format_timestamp_some() {
        let time = UNIX_EPOCH + Duration::from_secs(1737820245);
        let formatted = format_timestamp(Some(time));
        assert!(formatted.contains("2025-01-25"));
    }

    #[test]
    fn test_format_percentage() {
        assert_eq!(format_percentage(75, 100), "75.0%");
        assert_eq!(format_percentage(1, 3), "33.3%");
        assert_eq!(format_percentage(0, 100), "0.0%");
        assert_eq!(format_percentage(0, 0), "0.0%");
    }

    #[test]
    fn test_format_percentage_edge_cases() {
        // 100%
        assert_eq!(format_percentage(100, 100), "100.0%");

        // Rounding
        assert_eq!(format_percentage(2, 3), "66.7%");
        assert_eq!(format_percentage(1, 6), "16.7%");
    }

    #[test]
    fn test_format_backend_selections_json() {
        use std::collections::HashMap;

        let mut selections = HashMap::new();
        selections.insert("10.0.0.1:8080".to_string(), 75);
        selections.insert("10.0.0.2:8080".to_string(), 25);

        let backends = format_backend_selections_json(&selections, 100);
        assert_eq!(backends.len(), 2);

        // Verify structure (order may vary)
        for backend in &backends {
            assert!(backend.get("address").is_some());
            assert!(backend.get("selections").is_some());
            assert!(backend.get("percentage").is_some());
        }

        // Zero total case
        let backends_zero = format_backend_selections_json(&selections, 0);
        for backend in &backends_zero {
            assert_eq!(backend["percentage"], 0.0);
        }
    }
}

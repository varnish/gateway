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
}

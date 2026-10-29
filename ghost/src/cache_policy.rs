//! Cache policy compilation and evaluation.
//!
//! A `CachePolicy` (with optional per-rule overrides) is compiled once at config
//! load. At request time only the *request-side* conditions are evaluated; the
//! result is a bitmask carried (with the policy content key) in a
//! [`CacheDirective`] to the backend side, where status and response-header
//! conditions are evaluated to pick the final [`CacheSettings`].

use std::collections::hash_map::DefaultHasher;
use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

use crate::config::{
    parse_status_match, CacheKeyConfig, CachePolicy, CacheRuleOverride, MAX_CACHE_OVERRIDES,
};
use crate::director::{HeaderMatchCompiled, PathMatchCompiled, QueryParamMatchCompiled};

/// Request header carrying the JSON-encoded [`CacheDirective`] from `vcl_recv` to the backend side.
pub const CACHE_POLICY_HEADER: &str = "X-Ghost-Cache-Policy";
/// Request header carrying extra cache-key data from `vcl_recv` to `vcl_hash`.
pub const CACHE_KEY_EXTRA_HEADER: &str = "X-Ghost-Cache-Key-Extra";
/// Marker header set on ghost-generated synthetic responses (value: [`synthetic_token`]).
pub const SYNTHETIC_HEADER: &str = "X-Ghost-Synthetic";

/// Per-process random value of [`SYNTHETIC_HEADER`] on ghost-generated responses.
///
/// An origin cannot guess it, so it cannot opt its responses out of the cache
/// policy by sending the header itself.
pub fn synthetic_token() -> &'static str {
    static TOKEN: OnceLock<String> = OnceLock::new();
    TOKEN.get_or_init(|| format!("{:032x}", rand::random::<u128>()))
}
/// Duration of the hit-for-miss object created for uncacheable responses.
pub const HIT_FOR_MISS_SECONDS: f64 = 120.0;
/// How long a policy removed from the config stays resolvable (covers in-flight requests).
pub const POLICY_RETENTION: Duration = Duration::from_secs(300);

/// TTL semantics: `Default` applies only when the origin gives no Cache-Control; `Forced` always.
/// Negative values mean "do not cache".
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub enum Ttl {
    /// Used only when the origin sends no Cache-Control.
    Default(i64),
    /// Overrides origin caching headers.
    Forced(i64),
}

/// Effective cache settings after policy evaluation.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct CacheSettings {
    /// TTL (default or forced), if any.
    pub ttl: Option<Ttl>,
    /// Grace seconds, if set.
    pub grace: Option<u32>,
    /// Keep seconds, if set.
    pub keep: Option<u32>,
}

/// Serialized as JSON into [`CACHE_POLICY_HEADER`]. `k` = policy content key, `m` = request mask.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CacheDirective {
    /// Policy content key (see [`policy_key`]).
    pub k: u64,
    /// Request-side match mask: bit i set when override i's request conditions held.
    pub m: u64,
}

/// Content key: std `DefaultHasher` over `CachePolicy`'s derived Hash (deterministic in-process).
pub fn policy_key(cp: &CachePolicy) -> u64 {
    let mut h = DefaultHasher::new();
    cp.hash(&mut h);
    h.finish()
}

fn make_ttl(default: Option<i64>, forced: Option<i64>) -> Option<Ttl> {
    match (forced, default) {
        (Some(f), _) => Some(Ttl::Forced(f)),
        (None, Some(d)) => Some(Ttl::Default(d)),
        (None, None) => None,
    }
}

/// Method as seen by the cache: Varnish fetches a HEAD miss as GET and stores one
/// object for both, so rules must not distinguish them (the stored object's TTL
/// would otherwise depend on which method happened to miss first).
fn cache_method(method: &str) -> &str {
    if method == "HEAD" {
        "GET"
    } else {
        method
    }
}

/// One compiled override rule.
#[derive(Debug)]
struct CompiledOverride {
    path: Option<PathMatchCompiled>,
    method: Option<String>,
    request_headers: Vec<HeaderMatchCompiled>,
    query_params: Vec<QueryParamMatchCompiled>,
    status: Vec<(u16, u16)>,
    response_headers: Vec<HeaderMatchCompiled>,
    ttl: Option<Ttl>,
    grace: Option<u32>,
    keep: Option<u32>,
}

impl CompiledOverride {
    fn compile(o: &CacheRuleOverride) -> Result<Self, String> {
        let m = &o.match_;
        Ok(Self {
            path: m
                .path_match
                .as_ref()
                .map(PathMatchCompiled::from_config)
                .transpose()?,
            method: m.method.clone(),
            request_headers: m
                .request_headers
                .iter()
                .map(HeaderMatchCompiled::from_config)
                .collect::<Result<_, _>>()?,
            query_params: m
                .query_params
                .iter()
                .map(QueryParamMatchCompiled::from_config)
                .collect::<Result<_, _>>()?,
            status: m
                .status
                .iter()
                .map(|s| parse_status_match(s))
                .collect::<Result<_, _>>()?,
            response_headers: m
                .response_headers
                .iter()
                .map(HeaderMatchCompiled::from_config)
                .collect::<Result<_, _>>()?,
            ttl: make_ttl(o.default_ttl_seconds, o.forced_ttl_seconds),
            grace: o.grace_seconds,
            keep: o.keep_seconds,
        })
    }

    fn request_matches<'h>(
        &self,
        path: &str,
        method: &str,
        query: Option<&str>,
        header: &impl Fn(&str) -> Option<&'h str>,
    ) -> bool {
        if let Some(p) = &self.path {
            if !p.matches(path) {
                return false;
            }
        }
        if let Some(m) = &self.method {
            if cache_method(m) != cache_method(method) {
                return false;
            }
        }
        if !self
            .request_headers
            .iter()
            .all(|h| h.matches_value(header(h.name())))
        {
            return false;
        }
        match query {
            Some(q) => self.query_params.iter().all(|p| p.matches(q)),
            None => self.query_params.is_empty(),
        }
    }

    fn response_matches<'h>(&self, status: u16, header: &impl Fn(&str) -> Option<&'h str>) -> bool {
        (self.status.is_empty()
            || self
                .status
                .iter()
                .any(|&(lo, hi)| (lo..=hi).contains(&status)))
            && self
                .response_headers
                .iter()
                .all(|h| h.matches_value(header(h.name())))
    }
}

/// A cache policy compiled for fast evaluation.
#[derive(Debug)]
pub struct CompiledCachePolicy {
    /// Content key of the source policy.
    pub key: u64,
    /// Settings applied when no override matches.
    pub base: CacheSettings,
    /// Cache key configuration of the policy.
    pub cache_key: Option<CacheKeyConfig>,
    overrides: Vec<CompiledOverride>,
}

impl CompiledCachePolicy {
    /// Compile a policy (regexes, status ranges); `key = policy_key(cp)`.
    pub fn compile(cp: &CachePolicy) -> Result<Self, String> {
        if cp.overrides.len() > MAX_CACHE_OVERRIDES {
            return Err(format!(
                "too many cache overrides: {} (max {})",
                cp.overrides.len(),
                MAX_CACHE_OVERRIDES
            ));
        }
        Ok(Self {
            key: policy_key(cp),
            base: CacheSettings {
                ttl: make_ttl(cp.default_ttl_seconds, cp.forced_ttl_seconds),
                grace: cp.grace_seconds,
                keep: cp.keep_seconds,
            },
            cache_key: cp.cache_key.clone(),
            overrides: cp
                .overrides
                .iter()
                .map(CompiledOverride::compile)
                .collect::<Result<_, _>>()?,
        })
    }

    /// Bit i set when rule i's request-side conditions all hold (rules without any → always set).
    pub fn request_mask<'h>(
        &self,
        path: &str,
        method: &str,
        query: Option<&str>,
        header: impl Fn(&str) -> Option<&'h str>,
    ) -> u64 {
        self.overrides
            .iter()
            .enumerate()
            .filter(|(_, o)| o.request_matches(path, method, query, &header))
            .fold(0u64, |m, (i, _)| m | (1u64 << i))
    }

    /// First rule whose bit is in `mask` and whose status + response headers match.
    /// The TTL pair falls back to the base as a unit; grace and keep fall back individually.
    pub fn resolve<'h>(
        &self,
        mask: u64,
        status: u16,
        header: impl Fn(&str) -> Option<&'h str>,
    ) -> CacheSettings {
        for (i, o) in self.overrides.iter().enumerate() {
            if mask & (1u64 << i) != 0 && o.response_matches(status, &header) {
                return CacheSettings {
                    ttl: o.ttl.or(self.base.ttl),
                    grace: o.grace.or(self.base.grace),
                    keep: o.keep.or(self.base.keep),
                };
            }
        }
        self.base.clone()
    }
}

/// Lock-free-read table (held in ArcSwap by GhostDirector). Live entries have no expiry;
/// entries absent from the latest config carry the Instant they expire.
pub struct PolicyTable {
    map: HashMap<u64, (Arc<CompiledCachePolicy>, Option<Instant>)>,
}

impl PolicyTable {
    /// An empty table.
    pub fn empty() -> Self {
        Self {
            map: HashMap::new(),
        }
    }

    /// Live = `live`; keys in `prev` but not in `live` are kept with expiry
    /// (prev expiry if already retired, else now + retention), dropped once expired.
    pub fn rebuild(
        prev: &PolicyTable,
        live: Vec<Arc<CompiledCachePolicy>>,
        now: Instant,
        retention: Duration,
    ) -> PolicyTable {
        let mut map: HashMap<_, _> = live.into_iter().map(|p| (p.key, (p, None))).collect();
        for (k, (p, exp)) in &prev.map {
            if map.contains_key(k) {
                continue;
            }
            let expiry = exp.unwrap_or(now + retention);
            if now < expiry {
                map.insert(*k, (Arc::clone(p), Some(expiry)));
            }
        }
        PolicyTable { map }
    }

    /// Look up a policy by content key.
    pub fn get(&self, key: u64) -> Option<&Arc<CompiledCachePolicy>> {
        self.map.get(&key).map(|(p, _)| p)
    }
}

/// Unknown key → `CacheSettings { ttl: Some(Ttl::Forced(-1)), grace: None, keep: None }` (hit-for-miss).
pub fn select_settings<'h>(
    table: &PolicyTable,
    d: &CacheDirective,
    status: u16,
    header: impl Fn(&str) -> Option<&'h str>,
) -> CacheSettings {
    match table.get(d.k) {
        Some(p) => p.resolve(d.m, status, header),
        None => CacheSettings {
            ttl: Some(Ttl::Forced(-1)),
            grace: None,
            keep: None,
        },
    }
}

/// What to do with the backend response TTL.
#[derive(Debug, PartialEq)]
pub enum TtlAction {
    /// Keep whatever Varnish computed.
    Leave,
    /// Set beresp.ttl to this many seconds.
    Set(f64),
    /// Make a hit-for-miss object.
    HitForMiss,
}

/// Returns the action plus whether to strip Set-Cookie/Cache-Control/Expires (positive forced TTL only).
pub fn plan_ttl(ttl: Option<Ttl>, origin_has_cache_control: bool) -> (TtlAction, bool) {
    match ttl {
        None => (TtlAction::Leave, false),
        Some(Ttl::Forced(s)) if s < 0 => (TtlAction::HitForMiss, false),
        Some(Ttl::Forced(s)) => (TtlAction::Set(s as f64), true),
        Some(Ttl::Default(_)) if origin_has_cache_control => (TtlAction::Leave, false),
        Some(Ttl::Default(s)) if s < 0 => (TtlAction::HitForMiss, false),
        Some(Ttl::Default(s)) => (TtlAction::Set(s as f64), false),
    }
}

/// Everything `backend_response()` writes to beresp.
#[derive(Debug, PartialEq)]
pub struct BerespPlan {
    /// TTL action.
    pub ttl: TtlAction,
    /// Strip Set-Cookie/Cache-Control/Expires (positive forced TTL).
    pub strip: bool,
    /// Grace seconds to set, if any.
    pub grace: Option<u32>,
    /// Keep seconds to set, if any.
    pub keep: Option<u32>,
}

/// Turn effective settings into beresp writes. Hit-for-miss markers get no
/// policy grace/keep: like builtin.vcl's hit-for-miss they keep Varnish's
/// defaults, so a long policy keep can't pin marker objects in cache.
pub fn plan_beresp(settings: &CacheSettings, origin_has_cache_control: bool) -> BerespPlan {
    let (ttl, strip) = plan_ttl(settings.ttl, origin_has_cache_control);
    let (grace, keep) = if ttl == TtlAction::HitForMiss {
        (None, None)
    } else {
        (settings.grace, settings.keep)
    };
    BerespPlan {
        ttl,
        strip,
        grace,
        keep,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::*;
    use std::collections::HashMap;
    use std::time::{Duration, Instant};

    fn hm(name: &str, value: &str) -> HeaderMatch {
        HeaderMatch {
            name: name.to_string(),
            match_type: MatchType::Exact,
            value: value.to_string(),
        }
    }

    fn base_policy() -> CachePolicy {
        CachePolicy {
            default_ttl_seconds: Some(300),
            forced_ttl_seconds: None,
            grace_seconds: Some(30),
            keep_seconds: None,
            cache_key: None,
            bypass_headers: vec![],
            overrides: vec![],
        }
    }

    fn rule(m: CacheRuleMatch) -> CacheRuleOverride {
        CacheRuleOverride {
            match_: m,
            default_ttl_seconds: None,
            forced_ttl_seconds: None,
            grace_seconds: None,
            keep_seconds: None,
        }
    }

    fn status_rule(s: &str) -> CacheRuleOverride {
        rule(CacheRuleMatch {
            status: vec![s.to_string()],
            ..Default::default()
        })
    }

    fn no_headers(_: &str) -> Option<&'static str> {
        None
    }

    fn compile(cp: &CachePolicy) -> CompiledCachePolicy {
        CompiledCachePolicy::compile(cp).unwrap()
    }

    fn resolve_status(c: &CompiledCachePolicy, status: u16) -> CacheSettings {
        c.resolve(u64::MAX, status, no_headers)
    }

    #[test]
    fn test_first_match_wins() {
        let mut cp = base_policy();
        let mut r1 = status_rule("404");
        r1.forced_ttl_seconds = Some(1);
        let mut r2 = status_rule("4xx");
        r2.forced_ttl_seconds = Some(10);
        cp.overrides = vec![r1, r2];
        let c = compile(&cp);
        assert_eq!(resolve_status(&c, 404).ttl, Some(Ttl::Forced(1)));
        assert_eq!(resolve_status(&c, 403).ttl, Some(Ttl::Forced(10)));
    }

    #[test]
    fn test_no_match_uses_base() {
        let mut cp = base_policy();
        let mut r = status_rule("404");
        r.forced_ttl_seconds = Some(1);
        cp.overrides = vec![r];
        let c = compile(&cp);
        assert_eq!(resolve_status(&c, 200), c.base);
        assert_eq!(c.base.ttl, Some(Ttl::Default(300)));
        assert_eq!(c.base.grace, Some(30));
    }

    #[test]
    fn test_ttl_falls_back_as_unit() {
        let mut cp = base_policy();
        let mut r = status_rule("200");
        r.grace_seconds = Some(0);
        cp.overrides = vec![r];
        let s = resolve_status(&compile(&cp), 200);
        assert_eq!(s.ttl, Some(Ttl::Default(300)));
        assert_eq!(s.grace, Some(0));
    }

    #[test]
    fn test_ttl_rule_replaces_base_pair() {
        let mut cp = base_policy();
        cp.forced_ttl_seconds = Some(60);
        let mut r = status_rule("200");
        r.default_ttl_seconds = Some(5);
        cp.overrides = vec![r];
        let s = resolve_status(&compile(&cp), 200);
        assert_eq!(s.ttl, Some(Ttl::Default(5)));
    }

    #[test]
    fn test_grace_keep_fall_back_individually() {
        let mut cp = base_policy();
        cp.keep_seconds = Some(3600);
        let mut r = status_rule("200");
        r.keep_seconds = Some(0);
        cp.overrides = vec![r];
        let s = resolve_status(&compile(&cp), 200);
        assert_eq!(s.grace, Some(30));
        assert_eq!(s.keep, Some(0));
    }

    #[test]
    fn test_status_range_and_class() {
        let mut cp = base_policy();
        let mut r = status_rule("500-504");
        r.forced_ttl_seconds = Some(1);
        let mut r2 = status_rule("5xx");
        r2.forced_ttl_seconds = Some(2);
        cp.overrides = vec![r, r2];
        let c = compile(&cp);
        assert_eq!(resolve_status(&c, 503).ttl, Some(Ttl::Forced(1)));
        assert_eq!(resolve_status(&c, 505).ttl, Some(Ttl::Forced(2)));
        assert_eq!(resolve_status(&c, 599).ttl, Some(Ttl::Forced(2)));
        assert_eq!(resolve_status(&c, 200), c.base);
    }

    #[test]
    fn test_empty_status_matches_any() {
        let mut cp = base_policy();
        let mut r = rule(CacheRuleMatch::default());
        r.forced_ttl_seconds = Some(7);
        cp.overrides = vec![r];
        assert_eq!(resolve_status(&compile(&cp), 418).ttl, Some(Ttl::Forced(7)));
    }

    #[test]
    fn test_and_semantics() {
        let mut cp = base_policy();
        let mut r = rule(CacheRuleMatch {
            status: vec!["404".into()],
            response_headers: vec![hm("Content-Type", "video/mp2t")],
            ..Default::default()
        });
        r.forced_ttl_seconds = Some(1);
        cp.overrides = vec![r];
        let c = compile(&cp);
        let mut h: HashMap<&str, &str> = HashMap::new();
        h.insert("content-type", "text/html");
        assert_eq!(c.resolve(u64::MAX, 404, |n| h.get(n).copied()), c.base);
        h.insert("content-type", "video/mp2t");
        assert_eq!(
            c.resolve(u64::MAX, 404, |n| h.get(n).copied()).ttl,
            Some(Ttl::Forced(1))
        );
        assert_eq!(c.resolve(u64::MAX, 200, |n| h.get(n).copied()), c.base);
    }

    #[test]
    fn test_request_mask_bits() {
        let mut cp = base_policy();
        cp.overrides = vec![
            rule(CacheRuleMatch {
                path_match: Some(PathMatch {
                    match_type: PathMatchType::PathPrefix,
                    value: "/live/".into(),
                }),
                ..Default::default()
            }),
            rule(CacheRuleMatch::default()),
            rule(CacheRuleMatch {
                method: Some("POST".into()),
                ..Default::default()
            }),
        ];
        let c = compile(&cp);
        assert_eq!(c.request_mask("/live/a", "GET", None, no_headers), 0b011);
        assert_eq!(c.request_mask("/other", "POST", None, no_headers), 0b110);
    }

    #[test]
    fn test_request_mask_head_is_get() {
        // Varnish fetches a HEAD miss as GET and stores one object for both, so
        // method rules treat HEAD and GET alike.
        let mut cp = base_policy();
        cp.overrides = vec![
            rule(CacheRuleMatch {
                method: Some("GET".into()),
                ..Default::default()
            }),
            rule(CacheRuleMatch {
                method: Some("HEAD".into()),
                ..Default::default()
            }),
        ];
        let c = compile(&cp);
        assert_eq!(c.request_mask("/", "GET", None, no_headers), 0b11);
        assert_eq!(c.request_mask("/", "HEAD", None, no_headers), 0b11);
        assert_eq!(c.request_mask("/", "POST", None, no_headers), 0);
    }

    #[test]
    fn test_request_mask_request_headers() {
        let mut cp = base_policy();
        cp.overrides = vec![rule(CacheRuleMatch {
            request_headers: vec![hm("X-Foo", "bar")],
            ..Default::default()
        })];
        let c = compile(&cp);
        let mut h: HashMap<&str, &str> = HashMap::new();
        assert_eq!(c.request_mask("/", "GET", None, |n| h.get(n).copied()), 0);
        h.insert("x-foo", "bar");
        assert_eq!(c.request_mask("/", "GET", None, |n| h.get(n).copied()), 1);
    }

    #[test]
    fn test_request_mask_query_absent() {
        let mut cp = base_policy();
        cp.overrides = vec![rule(CacheRuleMatch {
            query_params: vec![QueryParamMatch {
                name: "a".into(),
                match_type: MatchType::Exact,
                value: "1".into(),
            }],
            ..Default::default()
        })];
        let c = compile(&cp);
        assert_eq!(c.request_mask("/", "GET", None, no_headers), 0);
        assert_eq!(c.request_mask("/", "GET", Some("a=2"), no_headers), 0);
        assert_eq!(c.request_mask("/", "GET", Some("a=1"), no_headers), 1);
    }

    #[test]
    fn test_resolve_respects_mask() {
        let mut cp = base_policy();
        let mut r = status_rule("200");
        r.forced_ttl_seconds = Some(1);
        cp.overrides = vec![r];
        let c = compile(&cp);
        assert_eq!(c.resolve(0b0, 200, no_headers), c.base);
        assert_eq!(c.resolve(0b1, 200, no_headers).ttl, Some(Ttl::Forced(1)));
    }

    #[test]
    fn test_too_many_overrides_rejected() {
        let mut cp = base_policy();
        cp.overrides = (0..MAX_CACHE_OVERRIDES + 1)
            .map(|_| rule(CacheRuleMatch::default()))
            .collect();
        assert!(CompiledCachePolicy::compile(&cp).is_err());
    }

    #[test]
    fn test_invalid_status_rejected() {
        let mut cp = base_policy();
        cp.overrides = vec![status_rule("9xx")];
        assert!(CompiledCachePolicy::compile(&cp).is_err());
    }

    #[test]
    fn test_policy_key_stable_and_sensitive() {
        let mut a = base_policy();
        a.overrides = vec![status_rule("404")];
        let b = a.clone();
        assert_eq!(policy_key(&a), policy_key(&b));
        let mut c = a.clone();
        c.overrides[0].grace_seconds = Some(5);
        assert_ne!(policy_key(&a), policy_key(&c));
    }

    fn table_with(p: &CachePolicy, t: Instant) -> PolicyTable {
        PolicyTable::rebuild(
            &PolicyTable::empty(),
            vec![Arc::new(compile(p))],
            t,
            POLICY_RETENTION,
        )
    }

    #[test]
    fn test_table_rebuild_unchanged_policy_stays_live() {
        let p = base_policy();
        let t0 = Instant::now();
        let t1 = table_with(&p, t0);
        let t2 = PolicyTable::rebuild(
            &t1,
            vec![Arc::new(compile(&p))],
            t0 + Duration::from_secs(1000),
            POLICY_RETENTION,
        );
        let k = policy_key(&p);
        assert!(t2.get(k).is_some());
        assert!(t2.map[&k].1.is_none());
    }

    #[test]
    fn test_table_rebuild_retains_removed() {
        let a = base_policy();
        let mut b = base_policy();
        b.grace_seconds = Some(1);
        let t0 = Instant::now();
        let t1 = table_with(&a, t0);
        let t2 = PolicyTable::rebuild(&t1, vec![Arc::new(compile(&b))], t0, POLICY_RETENTION);
        assert!(t2.get(policy_key(&a)).is_some());
        let at299 = t0 + Duration::from_secs(299);
        let t3 = PolicyTable::rebuild(&t2, vec![Arc::new(compile(&b))], at299, POLICY_RETENTION);
        assert!(
            t3.get(policy_key(&a)).is_some(),
            "expiry must not be extended"
        );
        let t4 = PolicyTable::rebuild(
            &t3,
            vec![Arc::new(compile(&b))],
            t0 + Duration::from_secs(301),
            POLICY_RETENTION,
        );
        assert!(t4.get(policy_key(&a)).is_none());
        assert!(t4.get(policy_key(&b)).is_some());
    }

    #[test]
    fn test_table_rebuild_readded_policy_is_live_again() {
        let a = base_policy();
        let mut b = base_policy();
        b.grace_seconds = Some(1);
        let t0 = Instant::now();
        let t1 = table_with(&a, t0);
        let t2 = PolicyTable::rebuild(&t1, vec![Arc::new(compile(&b))], t0, POLICY_RETENTION);
        let t3 = PolicyTable::rebuild(
            &t2,
            vec![Arc::new(compile(&a))],
            t0 + Duration::from_secs(10),
            POLICY_RETENTION,
        );
        assert!(t3.map[&policy_key(&a)].1.is_none());
    }

    #[test]
    fn test_table_rebuild_changed_policy_keeps_old_version() {
        let a = base_policy();
        let mut a2 = base_policy();
        a2.grace_seconds = Some(99);
        let t0 = Instant::now();
        let t1 = table_with(&a, t0);
        let t2 = PolicyTable::rebuild(&t1, vec![Arc::new(compile(&a2))], t0, POLICY_RETENTION);
        assert_eq!(t2.get(policy_key(&a)).unwrap().base.grace, Some(30));
        assert_eq!(t2.get(policy_key(&a2)).unwrap().base.grace, Some(99));
    }

    #[test]
    fn test_select_settings_unknown_key() {
        let s = select_settings(
            &PolicyTable::empty(),
            &CacheDirective { k: 42, m: 0 },
            200,
            no_headers,
        );
        assert_eq!(
            s,
            CacheSettings {
                ttl: Some(Ttl::Forced(-1)),
                grace: None,
                keep: None
            }
        );
    }

    #[test]
    fn test_select_settings_known_key() {
        let p = base_policy();
        let t = table_with(&p, Instant::now());
        let s = select_settings(
            &t,
            &CacheDirective {
                k: policy_key(&p),
                m: 0,
            },
            200,
            no_headers,
        );
        assert_eq!(s.ttl, Some(Ttl::Default(300)));
    }

    #[test]
    fn test_plan_ttl() {
        assert_eq!(
            plan_ttl(Some(Ttl::Forced(5)), true),
            (TtlAction::Set(5.0), true)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Forced(5)), false),
            (TtlAction::Set(5.0), true)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Forced(-1)), false),
            (TtlAction::HitForMiss, false)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Default(5)), true),
            (TtlAction::Leave, false)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Default(5)), false),
            (TtlAction::Set(5.0), false)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Default(-1)), true),
            (TtlAction::Leave, false)
        );
        assert_eq!(
            plan_ttl(Some(Ttl::Default(-1)), false),
            (TtlAction::HitForMiss, false)
        );
        assert_eq!(plan_ttl(None, false), (TtlAction::Leave, false));
    }

    #[test]
    fn test_plan_beresp_hit_for_miss_skips_grace_keep() {
        let s = |ttl| CacheSettings {
            ttl,
            grace: Some(30),
            keep: Some(86400),
        };
        let p = plan_beresp(&s(Some(Ttl::Forced(-1))), false);
        assert_eq!(p.ttl, TtlAction::HitForMiss);
        assert_eq!((p.grace, p.keep), (None, None));
        let p = plan_beresp(&s(Some(Ttl::Default(-1))), false);
        assert_eq!(p.ttl, TtlAction::HitForMiss);
        assert_eq!((p.grace, p.keep), (None, None));
        // Default TTL with origin Cache-Control: TTL left alone, grace/keep applied.
        let p = plan_beresp(&s(Some(Ttl::Default(-1))), true);
        assert_eq!(p.ttl, TtlAction::Leave);
        assert_eq!((p.grace, p.keep), (Some(30), Some(86400)));
        let p = plan_beresp(&s(Some(Ttl::Forced(60))), false);
        assert_eq!(
            p,
            BerespPlan {
                ttl: TtlAction::Set(60.0),
                strip: true,
                grace: Some(30),
                keep: Some(86400),
            }
        );
        let p = plan_beresp(&s(None), false);
        assert_eq!(p.ttl, TtlAction::Leave);
        assert_eq!((p.grace, p.keep), (Some(30), Some(86400)));
    }

    #[test]
    fn test_directive_json_roundtrip() {
        let d = CacheDirective {
            k: u64::MAX,
            m: 0b101,
        };
        let s = serde_json::to_string(&d).unwrap();
        let back: CacheDirective = serde_json::from_str(&s).unwrap();
        assert_eq!((back.k, back.m), (u64::MAX, 0b101));
    }
}

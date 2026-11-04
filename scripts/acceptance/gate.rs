// SPDX-License-Identifier: Apache-2.0
// Test-only gate: exercises hook lifecycle/settings; never makes routing policy.
use busbar_contract::abi::hook::{CLASS_GATE, PROMPT_NO, USER_NO};
use busbar_contract::abi::sdk::door::statement;
use busbar_contract::abi::sdk::hook::{statement_with_tail, tail, Hook, HookOpen};
use serde_json::{Map, Value};

fn validate(settings: &Map<String, Value>) -> Result<(), String> {
    match settings.get("threshold").and_then(Value::as_f64) {
        Some(value) if (0.0..=1.0).contains(&value) => Ok(()),
        _ => Err("threshold must be a number between zero and one".into()),
    }
}

struct Gate;
impl Hook for Gate {
    fn configure(&self, settings: &Map<String, Value>, _: u64) -> Result<(), String> {
        validate(settings)
    }
}

struct Open;
impl HookOpen for Open {
    fn open(settings: &str) -> Result<Box<dyn Hook>, String> {
        let settings = serde_json::from_str(settings).map_err(|error| error.to_string())?;
        validate(&settings)?;
        Ok(Box::new(Gate))
    }
}

busbar_contract::export_plugin! {
    hook: Open,
    statement: statement_with_tail(
        statement("tfacc-gate", env!("CARGO_PKG_VERSION"), 64),
        &tail(CLASS_GATE, PROMPT_NO, USER_NO),
    ),
}
busbar_contract::export_door!(door);

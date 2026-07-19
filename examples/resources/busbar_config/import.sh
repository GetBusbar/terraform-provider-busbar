# The config singleton has a fixed import id. The document is not recoverable
# (GET /config is a redacted projection), so supply the matching `document` in
# your configuration after import; only config_version is read live.
terraform import busbar_config.running config

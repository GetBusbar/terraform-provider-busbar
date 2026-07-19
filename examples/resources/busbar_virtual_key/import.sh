# Virtual keys are imported by their server-assigned id (the plaintext secret
# cannot be recovered — it is create-only — and stays null after import).
terraform import busbar_virtual_key.app vk_0123456789abcdef

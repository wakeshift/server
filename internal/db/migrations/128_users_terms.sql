-- Which Terms of Service a person accepted, and when (docs/agentic-onboarding.md,
-- "Record acceptance"): the sign-in page's notice ("By signing in, you
-- agree to Wakeshift's Terms of Service and Privacy Policy", Terms §2) is
-- accepted by creating an account, and accounts.signIn now records the
-- version shown (accounts.TermsVersion, the Terms' "Last updated" date)
-- with the time, for every new account. Additive: accounts made before
-- this have neither, and the previous revision, which doesn't write them,
-- still inserts users while it drains.
ALTER TABLE users ADD COLUMN IF NOT EXISTS terms_version text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS terms_accepted_at timestamptz;

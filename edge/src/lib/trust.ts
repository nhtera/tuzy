/**
 * Auto-trust: accounts at least this old, with no upheld abuse report, skip the browser warning
 * page on their tunnels (the same age boundary as auto-quarantine). Policy text on the website
 * (interstitial, landing, AUP) is built from AUTO_TRUST_DAYS so it can't drift.
 */
export const AUTO_TRUST_DAYS = 7;
export const AUTO_TRUST_SECONDS = AUTO_TRUST_DAYS * 24 * 3600;

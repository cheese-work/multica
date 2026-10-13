#!/usr/bin/env node
// Skeleton so the contract tests fail on behaviour, not on a missing module (CHE-1634).
const inert = () => ({ status: "missing", text: null, override: null, warnings: [] });
export const parseChangelogLine = inert;
export const sectionFor = () => "Improvements";
export const evaluatePr = () => ({ kind: "hidden", reason: "missing", warnings: [] });
export const formatCheck = () => ({ line: "", failing: false, warnings: [] });
export const generateEntries = () => ({ entries: [], hidden: [], warnings: [], counts: {} });
export const serializeEntries = () => "";
export const optionsFromEnv = () => ({});

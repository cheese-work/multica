// C1 Live key: <source>/<screen>/<state>/<profile>/<theme> (CHE-5793 plan r9).
const SOURCES = new Set(["route", "scene"]);

const fail = (code, message) => Object.assign(new Error(message), { code });

// profiles: [{ id, themes }] from design/devices.yml. Throws an Error with `.code`.
export function buildKey({ source, screen, state, profile, theme }, profiles) {
  const parts = { source, screen, state, profile, theme };
  for (const [name, v] of Object.entries(parts)) {
    if (typeof v !== "string" || !v || v.includes("/")) throw fail("invalid-key-part", `${name} must be a non-empty string without "/"`);
  }
  if (!SOURCES.has(source)) throw fail("invalid-key-part", `source must be route or scene, got ${source}`);
  const p = profiles.find((x) => x.id === profile);
  if (!p) throw fail("unknown-profile", `profile ${profile} is not in design/devices.yml`);
  if (!p.themes.includes(theme)) throw fail("unknown-theme", `theme ${theme} is not a theme of ${profile}`);
  return [source, screen, state, profile, theme].join("/");
}

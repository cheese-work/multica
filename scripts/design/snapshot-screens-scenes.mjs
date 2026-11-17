// Scene rules per route (CHE-6731): auth state, path, expected path and one landmark.
// A dynamic route is captured only when the e2e fixtures already seed a record for it
// (issue, project, runtime, member user id: see e2e/fixtures.ts); every other one is listed as not captured.
const heading = (text) => `:is(h1,h2,h3,[role=heading]):has-text("${text}")`;

// Route (without workspace slug) -> [screen, state, heading text, auth]. Static in-app routes.
const IN_APP = {
  "/issues": ["issues", "list", "All issues"],
  "/my-issues": ["my-issues", "empty", "My Issues"],
  "/inbox": ["inbox", "empty", "Inbox"],
  "/chat": ["chat", "no-agent", "No agent to chat with yet"],
  "/projects": ["projects", "list", "All projects"],
  "/agents": ["agents", "empty", "Hire your first AI teammate"],
  "/agents/new": ["agent-new", "runtime-step", "Where should this agent run?"],
  // /agents/new/manual redirects to /agents/new: the URL assertion lists it as not captured.
  "/agents/new/manual": ["agent-new-manual", "default", "Where should this agent run?"],
  "/agents/new/ai": ["agent-new-ai", "runtime-step", "Choose a runtime for Agent Builder"],
  "/squads": ["squads", "empty", "Put agents in a squad"],
  "/skills": ["skills", "empty", "Turn how your team works into skills"],
  "/runtimes": ["runtimes", "list", "All runtimes"],
  "/autopilots": ["autopilots", "empty", "Let agents start work on their own"],
  "/search": ["search", "default", "Command bar"],
  "/billing": ["billing", "test-page", "Billing (test page)"],
  "/usage": ["usage", "empty", "No usage yet"],
  "/views": ["views", "list", "All views"],
};

const SEEDED_DYNAMIC = {
  "/issues/[id]": (ids) => ({ path: `/issues/${ids.issue}`, screen: "issue-detail", state: "seeded", landmark: '[aria-current=page]:has-text("Snapshot issue")' }),
  "/projects/[id]": (ids) => ({ path: `/projects/${ids.project}`, screen: "project-detail", state: "seeded", landmark: heading("Snapshot project") }),
  "/runtimes/[id]": (ids) => ({ path: `/runtimes/${ids.runtime}`, screen: "runtime-detail", state: "seeded", landmark: ':text("Snapshot runtime")' }),
  "/members/[id]": (ids) => ({ path: `/members/${ids.member}`, screen: "member-detail", state: "seeded", landmark: heading("Snapshot Peer") }),
};

const AUTH = {
  "/login": { screen: "login", state: "logged-out", auth: "none", landmark: "input[type=email], input[name=email]" },
  "/invitations": { screen: "invitations", state: "empty", auth: "owner", landmark: heading("No pending invitations") },
  "/workspaces/new": { screen: "workspace-new", state: "default", auth: "owner", landmark: heading("Name your workspace") },
  "/onboarding": { screen: "onboarding", state: "step-1", auth: "fresh", landmark: "h1, h2, [role=heading]" },
};

export function scenes(r, { slug, ids, settingsTabs }) {
  if (r.scope === "auth") {
    if (AUTH[r.route]) return { captures: [{ ...AUTH[r.route], path: r.route }] };
    return { cause: `no e2e seed gives this route a record (${r.route})` };
  }
  if (r.scope !== "in-app") return { cause: "out of scope: not an in-app or (auth) route" };
  const sub = r.route.replace("/[workspaceSlug]", "");
  if (sub === "/settings") {
    return {
      captures: settingsTabs.visible.map((tab) => ({
        screen: `settings-${tab}`, state: "default", auth: "owner",
        path: `/${slug}/settings?tab=${tab}`, expectedPath: `/${slug}/settings`,
        landmark: `[data-testid="settings-nav-${tab}"]`, label: `${r.route}?tab=${tab}`,
      })),
      extra: settingsTabs.hidden.map((tab) => ({ route: `${r.route}?tab=${tab}`, cause: "tab not rendered for the seeded owner (feature flag off or role)" })),
    };
  }
  if (IN_APP[sub]) {
    const [screen, state, text] = IN_APP[sub];
    return { captures: [{ screen, state, auth: "owner", path: `/${slug}${sub}`, landmark: heading(text) }] };
  }
  if (SEEDED_DYNAMIC[sub]) {
    const c = SEEDED_DYNAMIC[sub](ids);
    return { captures: [{ ...c, auth: "owner", path: `/${slug}${c.path}` }] };
  }
  if (r.dynamic) return { cause: "dynamic route: no e2e seed gives it a record" };
  return { cause: "no scene rule for this static route" };
}

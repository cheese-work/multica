// The browser ceremony needs a real authenticator; the capture only needs a resolved assertion.
export async function signPasskeyChallenge() {
  return { id: "capture-credential", type: "public-key" };
}

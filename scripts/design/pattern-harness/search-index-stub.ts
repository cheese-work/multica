// Pickers and the editor mention menu import the search worker; the harness never searches and a Worker hangs headless Chrome on file://.
export async function searchIssues(): Promise<never[]> {
  return [];
}
export async function searchProjects(): Promise<never[]> {
  return [];
}

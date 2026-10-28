import { proxySearchApi } from "@/lib/search-proxy";

export async function GET(request: Request): Promise<Response> {
  return proxySearchApi(request, "/api/projects/search");
}

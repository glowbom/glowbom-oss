import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  index("routes/home.tsx"),
  route("glowby-oss", "routes/legacy-glowby-oss.tsx"),
  route("*", "routes/docs-page.tsx"),
] satisfies RouteConfig;

import { redirect } from "react-router";

export function loader() {
  return redirect("/glowbom-oss", 301);
}

export default function LegacyGlowbyOSSRoute() {
  return null;
}

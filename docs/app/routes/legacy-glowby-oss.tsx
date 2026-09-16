import { redirect } from "react-router";

export function loader() {
  return redirect(`${import.meta.env.BASE_URL}glowbom-oss`, 301);
}

export default function LegacyGlowbyOSSRoute() {
  return null;
}

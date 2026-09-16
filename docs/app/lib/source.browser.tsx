import type { ComponentProps, ComponentType, ReactElement } from "react";
import browserCollections from "fumadocs-mdx:collections/browser";

type BrowserDocsCollection = {
  createClientLoader: (options: {
    id: string;
    component: (loaded: { default: ComponentType }) => ReactElement;
  }) => {
    useContent: (path: string) => ReactElement;
  };
};

const docs = (browserCollections as any)?.docs;

function DocsLink({ href, ...props }: ComponentProps<"a">) {
  const resolvedHref = href?.startsWith("./")
    ? `${import.meta.env.BASE_URL}${href.slice(2)}`
    : href;
  return <a {...props} href={resolvedHref} />;
}

export const docsContent = docs.createClientLoader({
  id: "glowbom-docs",
  component: (loaded: { default: ComponentType }) => {
    const Content = loaded.default as ComponentType<{ components: { a: typeof DocsLink } }>;
    return <Content components={{ a: DocsLink }} />;
  },
});

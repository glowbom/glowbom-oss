import { loader } from "fumadocs-core/source";
import type { Folder, Item, Node, Root } from "fumadocs-core/page-tree";
import { docs } from "fumadocs-mdx:collections/server";
import type { LoadedDocPage, SearchRecord } from "./docs";

const baseUrl = "/";

function groupDesktopGuides(tree: Root): Root {
  const page = (url: string): Item => {
    const item = tree.children.find((node): node is Item => node.type === "page" && node.url === url);
    if (!item) throw new Error(`Missing documentation page: ${url}`);
    return item;
  };
  const desktop: Folder = {
    type: "folder",
    $id: "glowbom-desktop-guides",
    name: page("/desktop").name,
    index: page("/desktop"),
    defaultOpen: true,
    children: [page("/connect-ai"), page("/project-book"), page("/companion"), page("/glowbom-live"), page("/glowbom-oss"), page("/cli")],
  };
  const grouped = new Set(["/connect-ai", "/project-book", "/companion", "/glowbom-live", "/glowbom-oss", "/cli"]);

  // Group the guides without changing their existing public URLs.
  return {
    ...tree,
    children: tree.children.flatMap<Node>((node) => {
      if (node.type !== "page") return [node];
      if (node.url === "/desktop") return [desktop];
      return grouped.has(node.url) ? [] : [node];
    }),
  };
}

export const source = loader({
  baseUrl,
  source: docs.toFumadocsSource(),
  pageTree: { generateFallback: false, transformers: [{ root: groupDesktopGuides }] },
});

const treePromise = source.serializePageTree(source.pageTree);

export async function getSearchRecords() {
  return buildSearchRecords();
}

type StructuredData = {
  headings?: Array<{ id: string; content: string }>;
  contents?: Array<{ heading?: string; content: string }>;
};

type SearchPage = {
  path: string;
  url: string;
  data: {
    title?: string;
    description?: string;
    structuredData?: StructuredData | (() => Promise<StructuredData> | StructuredData);
    load?: () => Promise<{ structuredData?: StructuredData }>;
  };
};

async function getStructuredData(page: SearchPage): Promise<StructuredData | undefined> {
  if (page.data.structuredData) {
    return typeof page.data.structuredData === "function"
      ? await page.data.structuredData()
      : page.data.structuredData;
  }

  if (typeof page.data.load === "function") {
    return (await page.data.load()).structuredData;
  }

  return undefined;
}

function normalizeSearchText(value: string) {
  return value.replace(/\s+/g, " ").trim();
}

async function buildSearchRecords(): Promise<SearchRecord[]> {
  const pages = source.getPages() as SearchPage[];
  const records = await Promise.all(
    pages.map(async (page) => {
      const title = page.data.title ?? page.path;
      const description = page.data.description;
      const structuredData = await getStructuredData(page);
      const breadcrumbs = [title];
      const pageRecords: SearchRecord[] = [
        {
          id: page.url,
          url: page.url,
          type: "page",
          content: title,
          breadcrumbs: [],
          text: normalizeSearchText([title, description].filter(Boolean).join(" ")),
        },
      ];

      if (description) {
        pageRecords.push({
          id: `${page.url}#description`,
          url: page.url,
          type: "text",
          content: description,
          breadcrumbs,
          text: normalizeSearchText(`${title} ${description}`),
        });
      }

      for (const heading of structuredData?.headings ?? []) {
        pageRecords.push({
          id: `${page.url}#${heading.id}`,
          url: `${page.url}#${heading.id}`,
          type: "heading",
          content: heading.content,
          breadcrumbs,
          text: normalizeSearchText(`${title} ${heading.content}`),
        });
      }

      for (const [index, content] of (structuredData?.contents ?? []).entries()) {
        const text = normalizeSearchText(content.content);

        if (text.length === 0) {
          continue;
        }

        pageRecords.push({
          id: `${page.url}#text-${index}`,
          url: content.heading ? `${page.url}#${content.heading}` : page.url,
          type: "text",
          content: text,
          breadcrumbs,
          text: normalizeSearchText(`${title} ${text}`),
        });
      }

      return pageRecords;
    }),
  );

  return records.flat();
}

export async function loadDocPage(slugs?: string[]): Promise<LoadedDocPage> {
  const page = source.getPage(slugs);

  if (!page) {
    throw new Response("Not Found", { status: 404 });
  }

  return {
    tree: await treePromise,
    page: {
      path: page.path,
      title: page.data.title,
      description: page.data.description,
      toc: page.data.toc,
    },
    searchRecords: await getSearchRecords(),
  };
}

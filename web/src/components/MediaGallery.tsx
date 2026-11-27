import { useEffect, useState } from "react";

import { fetchMediaObjectURL } from "../api/client";

/** Images attached to one chunk, stored as a JSON array by the indexer. */
export function parseImageUrls(meta?: Record<string, string>): string[] {
  const raw = meta?.image_urls;
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) {
      return parsed.map((item) => String(item)).filter(Boolean);
    }
  } catch {
    // Older indexes may store a comma-separated list instead of JSON.
  }
  return raw
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

/**
 * Thumbnails for parsed figures with click-to-zoom. Images are fetched with
 * the bearer token because plain <img> requests cannot carry it.
 */
export default function MediaGallery({ urls }: { urls: string[] }) {
  const [objects, setObjects] = useState<string[]>([]);
  const [zoom, setZoom] = useState<string | null>(null);
  const key = urls.slice(0, 4).join("|");

  useEffect(() => {
    let active = true;
    const created: string[] = [];
    const targets = key ? key.split("|") : [];
    Promise.all(
      targets.map(async (path) => {
        try {
          const objectURL = await fetchMediaObjectURL(path);
          created.push(objectURL);
          return objectURL;
        } catch {
          return "";
        }
      }),
    ).then((items) => {
      if (active) {
        setObjects(items.filter(Boolean));
      } else {
        created.forEach((item) => URL.revokeObjectURL(item));
      }
    });
    return () => {
      active = false;
      created.forEach((item) => URL.revokeObjectURL(item));
    };
  }, [key]);

  if (objects.length === 0) return null;

  return (
    <>
      <div className="mt-2 flex flex-wrap gap-2">
        {objects.map((src, idx) => (
          <button
            key={src}
            type="button"
            onClick={() => setZoom(src)}
            className="overflow-hidden rounded-md border border-white/10 transition hover:border-accent/50"
            title="点击放大"
          >
            <img
              src={src}
              alt={`配图 ${idx + 1}`}
              className="h-16 w-24 object-cover"
              loading="lazy"
            />
          </button>
        ))}
      </div>
      {zoom && (
        <div
          role="presentation"
          onClick={() => setZoom(null)}
          className="fixed inset-0 z-50 flex cursor-zoom-out items-center justify-center bg-black/85 p-6"
        >
          <img
            src={zoom}
            alt="配图预览"
            className="max-h-[85vh] max-w-[90vw] rounded-lg shadow-2xl"
          />
        </div>
      )}
    </>
  );
}

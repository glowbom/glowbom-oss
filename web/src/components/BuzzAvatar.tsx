import { useEffect, useRef, useState } from 'react';
import type { BuzzMember } from '../lib/buzz-session';
import { withServerAuthHeaders } from '../lib/server-auth';

export function BuzzAvatar({ member, relayUrl }: { member: BuzzMember; relayUrl: string }) {
  const container = useRef<HTMLSpanElement>(null);
  const [source, setSource] = useState('');
  const [failed, setFailed] = useState(false);
  const initials = Array.from(member.displayName.trim()).slice(0, 2).join('').toUpperCase() || '?';

  useEffect(() => {
    setSource('');
    setFailed(false);
    if (!member.pictureUrl) return;
    let picture: URL;
    let relay: URL;
    try {
      picture = new URL(member.pictureUrl);
      relay = new URL(relayUrl);
      if (picture.protocol !== 'https:' || picture.username || picture.password) return;
    } catch { return; }

    if (picture.origin !== relay.origin || !picture.pathname.startsWith('/media/')) {
      // Public profile images do not receive the local token or Buzz credentials.
      setSource(picture.href);
      return;
    }

    const controller = new AbortController();
    let objectURL = '';
    let timeout: ReturnType<typeof setTimeout> | undefined;
    async function load() {
      timeout = setTimeout(() => controller.abort(), 20000);
      try {
        const response = await fetch('/api/buzz/session/avatar?pubkey=' + encodeURIComponent(member.pubkey), {
          headers: withServerAuthHeaders(), signal: controller.signal, cache: 'no-store',
        });
        if (!response.ok) throw new Error('Picture unavailable');
        const blob = await response.blob();
        if (controller.signal.aborted) return;
        if (blob.size > 2 * 1024 * 1024 || !['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(blob.type)) return;
        objectURL = URL.createObjectURL(blob);
        setSource(objectURL);
      } catch {
        // A missing picture must not interrupt the roster or its connection.
      } finally { clearTimeout(timeout); }
    }

    // Load only visible rows, including rows revealed by scrolling the roster.
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) {
        observer.disconnect();
        void load();
      }
    });
    if (container.current) observer.observe(container.current);
    return () => {
      observer.disconnect();
      controller.abort();
      clearTimeout(timeout);
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [member.pictureUrl, member.pubkey, relayUrl]);

  return <span ref={container} className="buzz-avatar" aria-hidden="true">
    {source && !failed
      ? <img src={source} alt="" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(true)} />
      : initials}
  </span>;
}

(() => {
  const parentOrigin = __GLOWBOM_PARENT_ORIGIN__;
  let capturing = false;
  let library;
  const loadLibrary = () => library || (library = new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = '/__glowbom_preview__/html2canvas-pro-2.4.5.js';
    script.onload = () => {
      const render = typeof window.html2canvas === 'function' ? window.html2canvas : window.html2canvas?.default;
      if (typeof render === 'function') resolve(render);
      else { library = undefined; reject(new Error('Capture unavailable.')); }
    };
    script.onerror = () => { library = undefined; reject(new Error('Could not load preview capture.')); };
    document.head.append(script);
  }));

  window.addEventListener('message', async (event) => {
    const request = event.data;
    if (event.source !== window.parent || event.origin !== parentOrigin || window.parent === window ||
        request?.type !== 'glowbom:preview:capture' || typeof request.id !== 'string' ||
        !/^[a-zA-Z0-9-]{8,80}$/.test(request.id) || capturing) return;
    capturing = true;
    const reply = (body) => window.parent.postMessage({type: 'glowbom:preview:captured', id: request.id, ...body}, parentOrigin);
    const marker = 'data-glowbom-capture-' + Math.random().toString(36).slice(2);
    const scrollers = [];
    const existingClones = new Set(document.querySelectorAll('iframe.html2canvas-container'));
    try {
      const render = await loadLibrary();
      const width = window.innerWidth;
      const height = window.innerHeight;
      const x = window.scrollX;
      const y = window.scrollY;
      if (width < 1 || height < 1 || width * height > 16000000) throw new Error('Resize the preview to capture a smaller area.');
      if (document.querySelector('iframe, video')) throw new Error('This preview contains a video or embedded frame that cannot be captured. Upload a screenshot to Draw instead.');
      for (const element of document.querySelectorAll('*')) {
        if (element.scrollLeft || element.scrollTop) {
          element.setAttribute(marker, String(scrollers.length));
          scrollers.push({element, x: element.scrollLeft, y: element.scrollTop});
        }
      }
      const scale = Math.min(window.devicePixelRatio || 1, 2, Math.sqrt(8000000 / (width * height)));
      const canvas = await render(document.documentElement, {
        x, y, scrollX: x, scrollY: y, width, height, windowWidth: width, windowHeight: height,
        scale, useCORS: true, allowTaint: false, logging: false, backgroundColor: '#ffffff',
        imageTimeout: 10000,
        onclone(clone) {
          const style = clone.createElement('style');
          style.textContent = '* { scroll-behavior: auto !important; scroll-snap-type: none !important; overflow-anchor: none !important; }';
          clone.documentElement.append(style);
          for (const element of clone.querySelectorAll('[' + marker + ']')) {
            const position = scrollers[Number(element.getAttribute(marker))];
            element.scrollTo({left: position.x, top: position.y, behavior: 'instant'});
            element.removeAttribute(marker);
          }
          clone.defaultView.scrollTo({left: x, top: y, behavior: 'instant'});
        }
      });
      const image = canvas.toDataURL('image/png');
      if (image.length > 20000000) throw new Error('The capture is too large. Resize the preview and try again.');
      reply({image, x, y, width, height});
    } catch (error) {
      // Never copy page errors or private URLs into the parent application.
      const known = error instanceof Error && /^(Resize the preview|This preview contains|Could not load preview capture|The capture is too large)/.test(error.message);
      reply({error: known ? error.message : 'Could not capture this preview. Some external images, canvas graphics, or page styles may not support capture. Upload a screenshot to Draw instead.'});
    } finally {
      for (const {element} of scrollers) element.removeAttribute(marker);
      // A renderer failure can leave its hidden clone behind and block the next capture.
      for (const clone of document.querySelectorAll('iframe.html2canvas-container')) {
        if (!existingClones.has(clone)) clone.remove();
      }
      capturing = false;
    }
  });
})();

// Simple HTTP server that wraps the @google/genai SDK for Veo video generation
// This reuses the exact same working code from the TypeScript prototype

const http = require('http');
const { GoogleGenAI } = require('@google/genai');

const PORT = 8081;

const server = http.createServer(async (req, res) => {
  // Set CORS headers
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'POST, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
  res.setHeader('Content-Type', 'application/json');

  if (req.method === 'OPTIONS') {
    res.writeHead(200);
    res.end();
    return;
  }

  if (req.method !== 'POST') {
    res.writeHead(405);
    res.end(JSON.stringify({ error: 'Method not allowed' }));
    return;
  }

  // Read request body
  let body = '';
  req.on('data', chunk => {
    body += chunk.toString();
  });

  req.on('end', async () => {
    try {
      const request = JSON.parse(body);

      if (req.url === '/generate') {
        await handleGenerate(request, res);
      } else if (req.url === '/poll') {
        await handlePoll(request, res);
      } else {
        res.writeHead(404);
        res.end(JSON.stringify({ error: 'Not found' }));
      }
    } catch (error) {
      console.error('Error:', error);
      res.writeHead(500);
      res.end(JSON.stringify({ error: error.message }));
    }
  });
});

async function handleGenerate(request, res) {
  const { prompt, images, aspectRatio, useKeyframes, extensionSource, geminiKey } = request;

  const ai = new GoogleGenAI({ apiKey: geminiKey });

  const isMultiImage = images.length > 1;
  const useKeyframesMode = useKeyframes && images.length === 2;

  // Choose model based on feature requirements (same as prototype)
  const model = (images.length > 2 || extensionSource)
    ? 'veo-3.1-generate-preview'
    : 'veo-3.1-fast-generate-preview';

  let operation;

  try {
    if (extensionSource) {
      // VIDEO EXTENSION
      operation = await ai.models.generateVideos({
        model: 'veo-3.1-generate-preview',
        prompt: prompt || "The action continues smoothly",
        video: extensionSource,
        config: {
          numberOfVideos: 1,
          resolution: '720p',
          aspectRatio: aspectRatio,
        }
      });
    } else if (useKeyframesMode) {
      // KEYFRAME INTERPOLATION
      operation = await ai.models.generateVideos({
        model: 'veo-3.1-fast-generate-preview',
        prompt: prompt,
        image: {
          imageBytes: images[0].data,
          mimeType: images[0].mimeType,
        },
        config: {
          numberOfVideos: 1,
          resolution: '720p',
          aspectRatio: aspectRatio,
          lastFrame: {
            imageBytes: images[1].data,
            mimeType: images[1].mimeType,
          }
        }
      });
    } else if (isMultiImage) {
      // MULTI-IMAGE ASSET REFERENCE
      const referenceImages = images.slice(0, 3).map(img => ({
        image: {
          imageBytes: img.data,
          mimeType: img.mimeType,
        },
        referenceType: "ASSET",
      }));

      operation = await ai.models.generateVideos({
        model: 'veo-3.1-generate-preview',
        prompt: prompt,
        config: {
          numberOfVideos: 1,
          resolution: '720p',
          aspectRatio: '16:9',
          referenceImages: referenceImages,
        }
      });
    } else {
      // SINGLE IMAGE
      operation = await ai.models.generateVideos({
        model: model,
        prompt: prompt,
        image: {
          imageBytes: images[0].data,
          mimeType: images[0].mimeType,
        },
        config: {
          numberOfVideos: 1,
          resolution: '720p',
          aspectRatio: aspectRatio
        }
      });
    }

    // Return operation ID
    res.writeHead(200);
    res.end(JSON.stringify({
      operationId: operation.name,
      message: "Video generation started successfully"
    }));

  } catch (error) {
    if (error.message?.includes("Requested entity was not found")) {
      res.writeHead(400);
      res.end(JSON.stringify({ error: "KEY_RESET_REQUIRED" }));
      return;
    }
    throw error;
  }
}

async function handlePoll(request, res) {
  const { operationId, geminiKey } = request;

  const ai = new GoogleGenAI({ apiKey: geminiKey });

  try {
    const operation = await ai.operations.getVideosOperation({
      operation: { name: operationId }
    });

    if (operation.error) {
      res.writeHead(200);
      res.end(JSON.stringify({
        done: true,
        status: "failed",
        error: operation.error.message || "Generation failed"
      }));
      return;
    }

    if (!operation.done) {
      res.writeHead(200);
      res.end(JSON.stringify({
        done: false,
        status: "processing"
      }));
      return;
    }

    // Operation completed
    const videoAsset = operation.response?.generatedVideos?.[0]?.video;

    if (!videoAsset || !videoAsset.uri) {
      res.writeHead(200);
      res.end(JSON.stringify({
        done: true,
        status: "failed",
        error: "No video URL returned. This may be due to safety filters."
      }));
      return;
    }

    res.writeHead(200);
    res.end(JSON.stringify({
      done: true,
      status: "completed",
      videoUrl: videoAsset.uri,
      videoAsset: {
        uri: videoAsset.uri,
        aspectRatio: videoAsset.aspectRatio || "16:9"
      }
    }));

  } catch (error) {
    if (error.message?.includes("Requested entity was not found")) {
      res.writeHead(400);
      res.end(JSON.stringify({ error: "KEY_RESET_REQUIRED" }));
      return;
    }
    throw error;
  }
}

server.listen(PORT, () => {
  console.log(`Veo bridge server running on http://localhost:${PORT}`);
  console.log(`Endpoints: POST /generate, POST /poll`);
});

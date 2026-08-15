#!/usr/bin/env node

// Veo Video Generation CLI Wrapper
// Wraps the TypeScript Veo service for use from Go backend

const { GoogleGenAI } = require("@google/genai");
const fs = require("fs");

async function generateVeoVideo(request) {
  const { prompt, images, aspectRatio, useKeyframes, extensionSource, geminiKey } = request;

  const ai = new GoogleGenAI({ apiKey: geminiKey });

  const isMultiImage = images.length > 1;
  const useKeyframesMode = useKeyframes && images.length === 2;

  // Choose model based on feature requirements
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
    return {
      operationId: operation.name,
      message: "Video generation started successfully"
    };

  } catch (error) {
    if (error.message?.includes("Requested entity was not found")) {
      throw new Error("KEY_RESET_REQUIRED");
    }
    throw error;
  }
}

async function pollVeoOperation(request) {
  const { operationId, geminiKey } = request;

  const ai = new GoogleGenAI({ apiKey: geminiKey });

  try {
    const operation = await ai.operations.getVideosOperation({
      operation: { name: operationId }
    });

    if (operation.error) {
      return {
        done: true,
        status: "failed",
        error: operation.error.message || "Generation failed"
      };
    }

    if (!operation.done) {
      return {
        done: false,
        status: "processing"
      };
    }

    // Operation completed
    const videoAsset = operation.response?.generatedVideos?.[0]?.video;

    if (!videoAsset || !videoAsset.uri) {
      return {
        done: true,
        status: "failed",
        error: "No video URL returned. This may be due to safety filters."
      };
    }

    return {
      done: true,
      status: "completed",
      videoUrl: videoAsset.uri,
      videoAsset: {
        uri: videoAsset.uri,
        aspectRatio: videoAsset.aspectRatio || "16:9"
      }
    };

  } catch (error) {
    if (error.message?.includes("Requested entity was not found")) {
      throw new Error("KEY_RESET_REQUIRED");
    }
    throw error;
  }
}

// CLI Entry Point
async function main() {
  const args = process.argv.slice(2);

  if (args.length === 0) {
    console.error("Usage: node veo-cli.js <generate|poll> <json-request>");
    process.exit(1);
  }

  const command = args[0];
  const requestJson = args[1];

  try {
    const request = JSON.parse(requestJson);
    let result;

    if (command === "generate") {
      result = await generateVeoVideo(request);
    } else if (command === "poll") {
      result = await pollVeoOperation(request);
    } else {
      throw new Error(`Unknown command: ${command}`);
    }

    console.log(JSON.stringify(result));
    process.exit(0);

  } catch (error) {
    console.error(JSON.stringify({ error: error.message }));
    process.exit(1);
  }
}

main();

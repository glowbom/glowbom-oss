# Jev for Glowbom OSS

Jev can help your OpenCode coding model make small, structured decisions while it works.

## 1. Install Jev as an OpenCode tool

From the root of `glowbom-oss`:

```bash
chmod +x extras/jev/install.sh
./extras/jev/install.sh
```

This installs the `jev` tool into your global OpenCode tools folder.

## 2. Restart Glowbom OSS and choose OpenCode

Restart Glowbom OSS:

```text
Ctrl+C
```

```bash
glowbom start
```

In Glowbom OSS, choose **OpenCode** and use any model that supports tool calls.

## 3. Add this instruction to your prompt

```text
Use Jev for narrow decisions with clear choices when it can save reasoning time.
```

That is it. Your coding model can now call Jev when a small decision is better handled as a clear set of choices.

### Optional

You can force Jev for a specific task by saying something like:

```text
Use the Jev tool to judge whether this build is healthy.
```

The current integration uses the free Jev endpoint through OpenCode Zen. No separate Jev API key is needed for this setup today.

# Jev for Glowbom OSS

Jev can help your OpenCode coding model make small, structured decisions while it works.

## Enable Jev in Glowbom

Open **Account → Settings → Build**. Glowbom checks Jev’s endpoint and shows **Available** when it responds. Turn on **Use Jev** to set up the tool automatically. An existing Jev tool is kept.

OpenCode builds check the endpoint again and load the tool before starting. If Jev is unavailable, the build continues without it. An active session is never restarted to load Jev.

## Manual setup

### 1. Install Jev as an OpenCode tool

From the root of `glowbom-oss`:

```bash
chmod +x extras/jev/install.sh
./extras/jev/install.sh
```

This installs the `jev` tool into your global OpenCode tools folder.

### 2. Restart Glowbom OSS and choose OpenCode

Restart Glowbom OSS:

```text
Ctrl+C
```

```bash
glowbom start
```

In Glowbom OSS, choose **OpenCode** and use any model that supports tool calls.

### 3. Add this instruction to your prompt

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

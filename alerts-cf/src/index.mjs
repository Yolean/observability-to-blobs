import { Container, getContainer } from "@cloudflare/containers";

// The alerting stack as one container instance: stock Prometheus in
// receiver mode + Alertmanager + the blobs-to-prometheus tailer.
export class AlertsStack extends Container {
  defaultPort = 9090;
  sleepAfter = "4h";

  constructor(ctx, env) {
    super(ctx, env);
    this.envVars = {
      S3_ENDPOINT: env.S3_ENDPOINT,
      S3_BUCKET: env.S3_BUCKET,
      CLUSTER_NAME: env.CLUSTER_NAME,
      S3_TLS: "on",
      S3_REGION: "auto",
      AWS_ACCESS_KEY_ID: env.R2_ACCESS_KEY_ID,
      AWS_SECRET_ACCESS_KEY: env.R2_SECRET_ACCESS_KEY,
    };
  }

  // The default implementation must stop the container when the activity
  // timeout expires; an empty override is the documented never-sleep.
  onActivityExpired() {}
}

// 60s alarm loop, armed by /watchdog/arm: the health probe doubles as the
// activity renewal, and DO alarms have at-least-once execution, which cron
// triggers do not.
export class Watchdog {
  constructor(ctx, env) {
    this.ctx = ctx;
    this.env = env;
  }

  async fetch() {
    await this.ctx.storage.setAlarm(Date.now() + 60_000);
    return new Response("watchdog armed\n");
  }

  async alarm() {
    try {
      const res = await getContainer(this.env.ALERTS).fetch("http://alerts/-/healthy");
      if (!res.ok) console.log("watchdog: prometheus unhealthy", res.status);
    } catch (e) {
      console.log("watchdog: stack unreachable", String(e));
    } finally {
      await this.ctx.storage.setAlarm(Date.now() + 60_000);
    }
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/watchdog/arm") {
      return env.WATCHDOG.get(env.WATCHDOG.idFromName("main")).fetch(request);
    }
    // Everything else is the Prometheus UI/API on the default port. No
    // auth yet: keep only demo data behind this until Cloudflare Access
    // fronts it.
    return getContainer(env.ALERTS).fetch(request);
  },
};

// import { PodDisruptor } from "k6/x/disruptor";
import { sleep } from "k6";
import http from "k6/http";
import { check } from "k6";
import { randomString } from "https://jslib.k6.io/k6-utils/1.2.0/index.js";
import { textSummary } from "https://jslib.k6.io/k6-summary/0.0.2/index.js";

export let options = {
  // iterations: 20,
  // vus: 5,
  scenarios: {
    // disrupt: {
    //   executor: "shared-iterations",
    //   iterations: 1,
    //   vus: 1,
    //   exec: "disrupt",
    //   startTime: "5s",
    // },
    // load: {
    //   executor: "constant-vus",
    //   vus: 5,
    //   duration: "30s",
    //   exec: "default",
    // },
    arrival: {
      executor: "constant-arrival-rate",
      // How long the test lasts
      duration: "20m",
      // How many iterations per timeUnit
      rate: 100,
      // Start `rate` iterations per second
      timeUnit: "1s",
      // Pre-allocate VUs
      preAllocatedVUs: 50,
      maxVUs: 5000,
    },
  },
};

// options = { iterations: 600, vus: 1 };

let address = "192.168.49.2:30033";
address = "localhost:80";

export function handleSummary(data) {
  let output = data;

  delete output.metrics;

  return {
    stdout: textSummary(output, { indent: " ", enableColors: true }),
  };
}

export function panic() {
  let addr = `http://${address}/leader`;
  // addr = `http://${address}/follower`;
  addr = `http://${address}/panic`;
  console.log(addr);
  let baseRes = http.get(addr);
  check(baseRes, {
    "Panic: status was 200": (r) =>
      r.status === 200 || console.error(`Base Error: Status was ${r.status}`),
  });

  return;
}
export default function () {
  // panic();
  // sleep(20);
  // return;

  // the key value to insert
  let key = randomString(15);
  let value = randomString(15);

  // key = "key1";
  // value = "value1";

  // Set a value to the map
  let setRes = http.get(`http://${address}/set?key=${key}&value=${value}`);

  check(setRes, {
    "Set: status was 200": (r) =>
      r.status === 200 || console.error(`Set Error: Status was ${r.status}`),
  });

  sleep(10);

  // Get a value from the map
  let getRes = http.get(`http://${address}/get?key=${key}`);

  check(getRes, {
    "Get: status was 200": (r) =>
      r.status === 200 ||
      console.error(`Get Error: Status was ${r.status} body = ${r.body}`),
    "Get: body contains testValue": (r) =>
      r.body.indexOf(value) !== -1 ||
      console.error(
        `Get Error: Body does not contain ${value}. body = ${r.body}`
      ),
  });

  // console.log("getRes.body", getRes.body);

  // // List all values from the map
  // let listRes = http.get(`http://${address}/list`);

  // console.log("listRes:", listRes.body);
  // check(listRes, {
  //   "List: status was 200": (r) => r.status === 200,
  // });
}

export function disrupt() {
  let panicRes = http.get(`http://${address}/panic`);

  check(panicRes, {
    "Panic: status was 502": (r) =>
      r.status === 502 || console.error(`Base Error: Status was ${r.status}`),
  });
}

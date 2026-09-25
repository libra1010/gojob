package com.gojob.example.quarkus;

import jakarta.enterprise.context.ApplicationScoped;

/** 被 HelloProcessor 注入的普通业务 bean，演示依赖注入能力。 */
@ApplicationScoped
public class GreetingService {

    public String greet(String jobParams) {
        return "hello from quarkus, jobParams=" + (jobParams == null ? "<none>" : jobParams);
    }
}

package com.gojob.worker.spring;

import java.lang.annotation.Documented;
import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

import org.springframework.stereotype.Component;

/**
 * 标注在 {@link com.gojob.worker.Processor} 实现类上：除按类全名自动注册外，
 * 再以 {@link #name()} 显式注册。Server 端 Job 的 processor 字段填该 name 即路由到此 Bean，
 * 适合不想在配置里写类全名的场景。
 *
 * <p>本注解元标注了 {@code @Component}，被标注的类会被组件扫描自动注册为 Spring Bean。
 */
@Target(ElementType.TYPE)
@Retention(RetentionPolicy.RUNTIME)
@Documented
@Component
public @interface GoJobProcessor {

    /** 注册名：Server 端 Job 的 processor 字段值。 */
    String name();
}

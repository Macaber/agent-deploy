package com.example.workspace.apiserver.controller;

import jakarta.servlet.http.Cookie;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.util.Map;

@RestController
@RequestMapping("/api")
@ConditionalOnProperty(name = "app.apiserver.enabled", havingValue = "true", matchIfMissing = true)
public class AuthController {

    @PostMapping("/login")
    public ResponseEntity<?> login(@RequestBody Map<String, String> body, HttpServletResponse response) {
        String username = body.get("username");
        String password = body.get("password");

        if (username == null || username.isBlank() || password == null || password.isBlank()) {
            return ResponseEntity.status(HttpStatus.UNAUTHORIZED).body(Map.of("error", "Username and password required"));
        }

        Cookie cookie = new Cookie("user_session", username);
        cookie.setPath("/");
        cookie.setMaxAge(24 * 3600);
        response.addCookie(cookie);

        return ResponseEntity.ok(Map.of("status", "success", "username", username));
    }

    @PostMapping("/logout")
    public ResponseEntity<?> logout(HttpServletResponse response) {
        Cookie cookie = new Cookie("user_session", "");
        cookie.setPath("/");
        cookie.setMaxAge(0);
        response.addCookie(cookie);

        return ResponseEntity.ok(Map.of("status", "logged_out"));
    }

    @GetMapping("/auth/check")
    public ResponseEntity<?> authCheck(HttpServletRequest request) {
        String username = getUsernameFromSession(request);
        if (username != null) {
            return ResponseEntity.ok(Map.of("authenticated", true, "username", username));
        }
        return ResponseEntity.ok(Map.of("authenticated", false));
    }

    private String getUsernameFromSession(HttpServletRequest request) {
        if (request.getCookies() != null) {
            for (Cookie c : request.getCookies()) {
                if ("user_session".equals(c.getName()) && !c.getValue().isBlank()) {
                    return c.getValue();
                }
            }
        }
        return null;
    }
}
